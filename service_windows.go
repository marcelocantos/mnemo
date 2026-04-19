// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Windows Service support for mnemo.
//
// There are three entry points, each wired up from main.go:
//
//   - runAsServiceIfUnderSCM: called on every foreground launch.
//     Detects whether the Service Control Manager started the
//     process (svc.IsWindowsService) and, if so, hands off to the
//     service dispatcher. Returns (false, nil) on interactive
//     launches so main.go continues its normal path.
//
//   - installService: creates the "mnemo" service, configures
//     auto-start and restart-on-failure, and registers an Event Log
//     source. Called by `mnemo install-service` from the installer.
//
//   - uninstallService: stops and deletes the service and its Event
//     Log source. Called by `mnemo uninstall-service` from the
//     uninstaller.
//
// Service logs are written via the Windows Event Log (visible in
// Event Viewer under Windows Logs → Application with source "mnemo"),
// and stderr from runServe is also redirected to
// %ProgramData%\mnemo\logs\mnemo.log so operators have a readable
// file for deep debugging.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// runAsServiceIfUnderSCM checks whether the current process was
// started by the Service Control Manager. If so, it runs the service
// dispatcher (which calls serviceMain and ultimately runServe). If
// not, it returns (false, nil) so the caller can continue with its
// interactive/foreground path.
func runAsServiceIfUnderSCM(addr string) (bool, error) {
	underSCM, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("detect windows service: %w", err)
	}
	if !underSCM {
		return false, nil
	}

	// Redirect stderr (where slog writes) to a log file under
	// %ProgramData%\mnemo\logs. This survives service restarts and
	// gives operators a file they can open without Event Viewer.
	if logFile, err := openServiceLogFile(); err == nil {
		os.Stderr = logFile
	}

	return true, svc.Run(serviceName, &mnemoService{addr: addr})
}

// mnemoService implements svc.Handler.
type mnemoService struct {
	addr string
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
	go func() { done <- runServe(ctx, m.addr) }()

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

// openServiceLogFile opens (or creates) the service log file under
// %ProgramData%\mnemo\logs\mnemo.log in append mode. Returns an error
// if the directory cannot be created or the file cannot be opened;
// callers treat that as non-fatal (stderr just stays as the SCM's
// default, which is the null device).
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

// installService creates the mnemo Windows Service pointing at the
// running executable. It configures auto-start-on-boot and
// restart-on-failure, and registers an Event Log source named
// "mnemo". Idempotent: succeeds without error if the service already
// exists.
func installService(args []string) error {
	fs := flag.NewFlagSet("install-service", flag.ExitOnError)
	exePath := fs.String("exe", "", "path to mnemo.exe (default: current executable)")
	_ = fs.Parse(args)

	path, err := resolveExe(*exePath)
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w (this command must run as Administrator)", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(serviceName)
	if err == nil {
		// Already installed. Update the exe path in case the user
		// reinstalled into a different location.
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

	// Configure automatic restart on unexpected exit: restart after
	// 10 seconds, reset failure counter after an hour of clean run.
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
		// Already-registered sources return an error we don't care
		// about; tolerate.
		fmt.Fprintf(os.Stderr, "warning: event log install: %v\n", err)
	}

	fmt.Printf("service %q installed (exe=%s)\n", serviceName, path)
	return nil
}

// uninstallService stops (if running) and deletes the mnemo service
// and its Event Log source. Idempotent: returns no error if the
// service is already gone.
func uninstallService(args []string) error {
	_ = args

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w (this command must run as Administrator)", err)
	}
	defer func() { _ = m.Disconnect() }()

	s, err := m.OpenService(serviceName)
	if err != nil {
		// Already gone — still try to drop the event log source.
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
		// Wait up to 15 seconds for the service to stop before
		// deleting it. Delete will still be queued if it hasn't
		// stopped, but this gives the handler a chance to drain.
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
		// ERROR_SERVICE_MARKED_FOR_DELETE is fine — it means a prior
		// delete is still pending.
		if err != windows.ERROR_SERVICE_MARKED_FOR_DELETE {
			return fmt.Errorf("delete service: %w", err)
		}
	}
	if err := eventlog.Remove(serviceName); err != nil {
		// Non-fatal.
		fmt.Fprintf(os.Stderr, "warning: event log remove: %v\n", err)
	}
	fmt.Printf("service %q uninstalled\n", serviceName)
	return nil
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

