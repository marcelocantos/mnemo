// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Windows Service support for mnemo.
//
// mnemo runs as a long-lived Windows Service (auto-start on boot,
// restart on failure, survives logoff / battery / sleep). Because a
// service runs as LocalSystem by default — whose `os.UserHomeDir()`
// returns `C:\Windows\System32\config\systemprofile`, NOT the
// installing user's home — mnemo cannot assume an implicit user
// identity on Windows. Instead, the daemon stays idle until the
// first MCP request arrives carrying `?user=<name>`; the Registry
// then spins up that user's transcript tree, database, and
// background workers.
//
// Three entry points, wired from main.go:
//
//   - runAsServiceIfUnderSCM: called on every foreground launch.
//     Detects SCM-started processes via svc.IsWindowsService and
//     hands off to the dispatcher. Returns (false, nil) on
//     interactive launches so main.go continues its normal path.
//
//   - installService: creates the "mnemo" service, configures
//     auto-start and restart-on-failure, registers an Event Log
//     source, and tears down any legacy v0.23–v0.24 Scheduled Task
//     of the same name (for clean upgrades in place).
//
//   - uninstallService: stops (with drain) and deletes the service
//     and its Event Log source. Also cleans up any legacy Task.
//
// Service logs go to the Windows Event Log (visible in Event Viewer
// under Application, source "mnemo") plus
// %ProgramData%\mnemo\logs\mnemo.log for readable file logs.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName is the Windows Service identifier. Chosen bare ("mnemo")
// so users see a clean entry in services.msc and `sc query mnemo`.
const serviceName = "mnemo"

// runAsServiceIfUnderSCM checks whether the current process was
// started by the Service Control Manager. If so, it runs the service
// dispatcher (which calls serviceMain and ultimately runServe). If
// not, it returns (false, nil) so the caller can continue with its
// interactive/foreground path.
func runAsServiceIfUnderSCM(addr, federatedAddr string) (bool, error) {
	underSCM, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("detect windows service: %w", err)
	}
	if !underSCM {
		return false, nil
	}

	// Redirect stderr (slog's sink) to a file under %ProgramData%
	// so operators have a readable log independent of Event Viewer.
	if logFile, err := openServiceLogFile(); err == nil {
		os.Stderr = logFile
	}
	return true, svc.Run(serviceName, &mnemoService{addr: addr, federatedAddr: federatedAddr})
}

type mnemoService struct {
	addr          string
	federatedAddr string
}

func (m *mnemoService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	elog, err := eventlog.Open(serviceName)
	if err == nil {
		defer func() { _ = elog.Close() }()
		_ = elog.Info(1, fmt.Sprintf("mnemo %s service starting on %s", version, m.addr))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runServe(ctx, m.addr, m.federatedAddr) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepts}

loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if elog != nil {
					_ = elog.Info(1, "mnemo service stopping")
				}
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					if elog != nil {
						_ = elog.Warning(1, "runServe did not exit within 10s; forcing stop")
					}
				}
				break loop
			default:
				if elog != nil {
					_ = elog.Warning(1, fmt.Sprintf("unexpected SCM request: %d", c.Cmd))
				}
			}
		case err := <-done:
			if err != nil && elog != nil {
				_ = elog.Error(1, fmt.Sprintf("runServe exited with error: %v", err))
			}
			break loop
		}
	}

	changes <- svc.Status{State: svc.Stopped}
	return false, 0
}

// openServiceLogFile opens (or creates) %ProgramData%\mnemo\logs\mnemo.log
// in append mode. Non-fatal if the open fails — stderr just stays
// as the SCM's default (the null device).
func openServiceLogFile() (*os.File, error) {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	logDir := filepath.Join(base, "mnemo", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(logDir, "mnemo.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// installService creates the mnemo service (auto-start, restart on
// failure, Event Log source). Idempotent — an existing install has
// its exe path and start-type refreshed in place.
func installService(args []string) error {
	fs := flag.NewFlagSet("install-service", flag.ExitOnError)
	exePath := fs.String("exe", "", "path to mnemo.exe (default: current executable)")
	_ = fs.Parse(args)

	path, err := resolveExe(*exePath)
	if err != nil {
		return err
	}

	// Upgrades over v0.23.x / v0.24.x replace the Scheduled Task
	// with a real Service. Tear the old Task down before installing
	// the Service so it doesn't double-run mnemo at next logon.
	cleanupLegacyScheduledTask()

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w (this command must run as Administrator)", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(serviceName)
	if err == nil {
		defer func() { _ = s.Close() }()
		cfg, err := s.Config()
		if err != nil {
			return fmt.Errorf("read existing service config: %w", err)
		}
		cfg.BinaryPathName = path
		cfg.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("update service config: %w", err)
		}
		fmt.Printf("service %q updated (exe=%s)\n", serviceName, path)
		return nil
	}

	s, err = m.CreateService(serviceName, path, mgr.Config{
		DisplayName: "mnemo MCP server",
		Description: "Searchable memory across Claude Code session transcripts.",
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer func() { _ = s.Close() }()

	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(recovery, 3600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set recovery actions: %v\n", err)
	}

	if err := eventlog.InstallAsEventCreate(serviceName,
		eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		fmt.Fprintf(os.Stderr, "warning: event log install: %v\n", err)
	}

	fmt.Printf("service %q installed (exe=%s)\n", serviceName, path)
	return nil
}

// uninstallService stops (with 15-second drain), deletes, and cleans
// up Event Log. Also tears down any legacy Scheduled Task.
func uninstallService(args []string) error {
	_ = args
	cleanupLegacyScheduledTask()

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w (this command must run as Administrator)", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(serviceName)
	if err != nil {
		_ = eventlog.Remove(serviceName)
		fmt.Printf("service %q not installed\n", serviceName)
		return nil
	}
	defer func() { _ = s.Close() }()

	status, err := s.Query()
	if err == nil && status.State != svc.Stopped && status.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not send stop: %v\n", err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			status, err = s.Query()
			if err != nil || status.State == svc.Stopped {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}

	if err := s.Delete(); err != nil {
		if err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
			return fmt.Errorf("delete service: %w", err)
		}
	}
	if err := eventlog.Remove(serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: event log remove: %v\n", err)
	}
	fmt.Printf("service %q uninstalled\n", serviceName)
	return nil
}

// cleanupLegacyScheduledTask removes the "mnemo" Scheduled Task left
// behind by v0.23.0 / v0.24.0 installs. Best-effort — a missing
// task is the expected fresh-install path and produces no output.
func cleanupLegacyScheduledTask() {
	end := exec.Command("schtasks.exe", "/end", "/tn", serviceName)
	_, _ = end.CombinedOutput()
	del := exec.Command("schtasks.exe", "/delete", "/tn", serviceName, "/f")
	if out, err := del.CombinedOutput(); err == nil {
		fmt.Printf("legacy Scheduled Task %q removed\n", serviceName)
		_ = out
	}
}

func resolveExe(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve --exe: %w", err)
		}
		return abs, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	return path, nil
}
