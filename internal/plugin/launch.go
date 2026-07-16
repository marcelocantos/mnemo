// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marcelocantos/mnemo/internal/breaker"
	"github.com/marcelocantos/mnemo/internal/store"
)

// Launch handshake (docs/design/plugin-system.md §4, 🎯T102.4):
//
//	MNEMO_PLUGIN_PORT <port>\n
//
// The child binds 127.0.0.1:ephemeral, prints that line on stdout, then
// serves the same HTTP contract as connect-mode (/ready, /manifest, …).
// mnemo builds http://127.0.0.1:<port> and runs AttachConnect.
//
// Param injection: each plugins[].params key K is exposed as
//
//	MNEMO_PLUGIN_PARAM_<KEY>=<value>
//
// where KEY is K upper-cased with '-' → '_'. Values are fmt.Sprint for
// scalars; nested objects/arrays are JSON-encoded. Also set:
//
//	MNEMO_PLUGIN_NAME=<config name>
//	MNEMO_PLUGIN_HOME=<~/.mnemo/plugins/<name>>
const (
	handshakePrefix = "MNEMO_PLUGIN_PORT "

	defaultHandshakeTimeout = 10 * time.Second
	defaultStopGrace        = 5 * time.Second
	defaultLaunchBackoffMin = 200 * time.Millisecond
	defaultLaunchBackoffMax = 30 * time.Second
	// Trip after this many consecutive failed start/crash cycles.
	defaultLaunchBreakerThreshold = 5
	defaultLaunchBreakerCooldown  = 5 * time.Minute
)

// launchConfig tunes the supervisor. Zero values mean defaults; tests
// inject short timeouts via struct params (no functional options).
type launchConfig struct {
	HandshakeTimeout time.Duration
	StopGrace        time.Duration
	BackoffMin       time.Duration
	BackoffMax       time.Duration
	BreakerThreshold int
	BreakerCooldown  time.Duration
}

func (c launchConfig) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return defaultHandshakeTimeout
}

func (c launchConfig) stopGrace() time.Duration {
	if c.StopGrace > 0 {
		return c.StopGrace
	}
	return defaultStopGrace
}

func (c launchConfig) backoffMin() time.Duration {
	if c.BackoffMin > 0 {
		return c.BackoffMin
	}
	return defaultLaunchBackoffMin
}

func (c launchConfig) backoffMax() time.Duration {
	if c.BackoffMax > 0 {
		return c.BackoffMax
	}
	return defaultLaunchBackoffMax
}

func (c launchConfig) breakerThreshold() int {
	if c.BreakerThreshold > 0 {
		return c.BreakerThreshold
	}
	return defaultLaunchBreakerThreshold
}

func (c launchConfig) breakerCooldown() time.Duration {
	if c.BreakerCooldown > 0 {
		return c.BreakerCooldown
	}
	return defaultLaunchBreakerCooldown
}

// launchSupervisor owns one plugin child process: spawn, stdout port
// handshake, AttachConnect, crash restart with backoff, and T84 breaker
// on persistent failure. Patterns mirror shimSupervisor lifecycle and
// the compaction watcher's breaker usage.
type launchSupervisor struct {
	name    string
	entry   store.PluginEntry
	cmdPath string
	home    string // plugin home path
	client  *http.Client
	log     *slog.Logger
	cfg     launchConfig
	br      *breaker.Breaker

	// mgr+inst used only for state updates; always check inst.launch == s
	// under mgr.mu before mutating so a replaced/stopped supervisor is inert.
	mgr  *Manager
	inst *Instance

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}

	procMu   sync.Mutex
	cmd      *exec.Cmd
	waitDone chan error // closed/sent exactly once by the reaper goroutine
}

func newLaunchSupervisor(mgr *Manager, inst *Instance, cmdPath string, cfg launchConfig) *launchSupervisor {
	return &launchSupervisor{
		name:    inst.Name,
		entry:   inst.Entry,
		cmdPath: cmdPath,
		home:    inst.Home,
		client:  mgr.client,
		log:     mgr.log.With("plugin", inst.Name, "transport", "launch"),
		cfg:     cfg,
		br:      breaker.New(cfg.breakerThreshold(), cfg.breakerCooldown()),
		mgr:     mgr,
		inst:    inst,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// startOnce spawns the child, reads the port handshake, and attaches.
// On failure the child is reaped. Does not record breaker outcomes —
// the caller (first start or restart loop) does.
func (s *launchSupervisor) startOnce(ctx context.Context) (*AttachResult, error) {
	if s.cmdPath == "" {
		return nil, fmt.Errorf("command is empty")
	}
	s.killAndReap()

	cmd := exec.Command(s.cmdPath, s.entry.Args...) //nolint:gosec // path from user config
	cmd.Env = buildPluginEnv(s.name, s.home, s.entry.Params)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Discard stderr so a full pipe never blocks the child.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.cmdPath, err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	s.procMu.Lock()
	s.cmd = cmd
	s.waitDone = waitDone
	s.procMu.Unlock()

	port, err := s.readHandshake(stdout)
	if err != nil {
		s.killAndReap()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	// Drain remaining stdout so a chatty plugin cannot block on a full pipe.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	att, err := AttachConnect(ctx, s.client, baseURL, s.name)
	if err != nil {
		s.killAndReap()
		return nil, err
	}
	return att, nil
}

// run is the restart loop. Call after the first startOnce.
// firstOK is whether that first start attached successfully. Exits when
// Stop is invoked. Exclusive owner of waitDone reaping while running —
// Stop only signals; this loop reaps and closes doneCh.
func (s *launchSupervisor) run(firstOK bool) {
	defer close(s.doneCh)
	defer s.killAndReap()
	backoff := s.cfg.backoffMin()

	// If the first start succeeded, block until the child exits (crash)
	// or stop is requested. A failed first start falls straight into the
	// retry path (first failure already recorded on the breaker).
	if firstOK {
		waitErr := s.waitChild()
		if s.stopped() {
			return
		}
		msg := "plugin process exited"
		if waitErr != nil {
			msg = waitErr.Error()
		}
		s.br.Record(time.Now(), false, msg)
		s.mgr.noteLaunchError(s.inst, s, msg)
		s.log.Warn("plugin exited; will restart", "err", msg, "backoff", backoff)
	}

	for {
		if !s.waitBreakerOrStop() {
			return
		}
		if !s.sleepOrStop(backoff) {
			return
		}
		if backoff < s.cfg.backoffMax() {
			backoff *= 2
			if backoff > s.cfg.backoffMax() {
				backoff = s.cfg.backoffMax()
			}
		}

		att, err := s.startOnce(context.Background())
		if err != nil {
			if s.stopped() {
				return
			}
			s.br.Record(time.Now(), false, err.Error())
			s.mgr.noteLaunchError(s.inst, s, err.Error())
			s.log.Warn("plugin restart failed", "err", err)
			continue
		}
		s.br.Record(time.Now(), true, "")
		backoff = s.cfg.backoffMin()
		s.mgr.noteLaunchReady(s.inst, s, att)
		s.log.Info("plugin restarted", "base_url", att.BaseURL, "version", att.Manifest.Version)

		waitErr := s.waitChild()
		if s.stopped() {
			return
		}
		msg := "plugin process exited"
		if waitErr != nil {
			msg = waitErr.Error()
		}
		s.br.Record(time.Now(), false, msg)
		s.mgr.noteLaunchError(s.inst, s, msg)
		s.log.Warn("plugin exited; will restart", "err", msg, "backoff", backoff)
	}
}

// Stop signals the loop and kills the child so waitChild unblocks, then
// waits for the loop to exit. Does not reap waitDone itself (run owns
// reaping) — avoids a double-Wait deadlock. Idempotent.
func (s *launchSupervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.signalKill()
	<-s.doneCh
}

func (s *launchSupervisor) stopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

func (s *launchSupervisor) sleepOrStop(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.stopCh:
		return false
	case <-t.C:
		return true
	}
}

// waitBreakerOrStop blocks while the breaker is open (within cooldown),
// returning false if stop is requested. When cooldown elapses Allow
// flips to half-open and we return true so the loop can retry.
func (s *launchSupervisor) waitBreakerOrStop() bool {
	for {
		if s.stopped() {
			return false
		}
		if s.br.Allow(time.Now()) {
			return true
		}
		snap := s.br.Snapshot()
		s.mgr.noteLaunchError(s.inst, s, fmt.Sprintf("circuit breaker open: %s", snap.LastError))
		s.log.Warn("plugin circuit breaker open; backing off",
			"last_error", snap.LastError, "trip_count", snap.TripCount)
		// Poll so we notice stop and cooldown without a long uninterruptible sleep.
		if !s.sleepOrStop(time.Second) {
			return false
		}
	}
}

func (s *launchSupervisor) readHandshake(r io.Reader) (int, error) {
	type result struct {
		port int
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		port, err := scanHandshake(r)
		ch <- result{port, err}
	}()
	select {
	case <-s.stopCh:
		return 0, fmt.Errorf("stopped during handshake")
	case <-time.After(s.cfg.handshakeTimeout()):
		return 0, fmt.Errorf("timeout after %s waiting for %q line", s.cfg.handshakeTimeout(), strings.TrimSpace(handshakePrefix))
	case res := <-ch:
		return res.port, res.err
	}
}

// scanHandshake reads lines until MNEMO_PLUGIN_PORT <port>.
func scanHandshake(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	// Port line is short; keep default token size.
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, handshakePrefix) {
			continue
		}
		portStr := strings.TrimSpace(strings.TrimPrefix(line, handshakePrefix))
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return 0, fmt.Errorf("parse port %q: %w", portStr, err)
		}
		if port < 1 || port > 65535 {
			return 0, fmt.Errorf("port %d out of range", port)
		}
		return port, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("stdout closed before %q line", strings.TrimSpace(handshakePrefix))
}

// waitChild blocks until the current child exits or stop is requested.
// Exclusive reaper of waitDone while the supervise loop is active.
func (s *launchSupervisor) waitChild() error {
	s.procMu.Lock()
	ch := s.waitDone
	s.procMu.Unlock()
	if ch == nil {
		return nil
	}
	// Prefer stop: signal graceful terminate, then reap.
	select {
	case <-s.stopCh:
		s.signalGraceful()
		select {
		case err := <-ch:
			s.clearProc()
			return err
		case <-time.After(s.cfg.stopGrace()):
			s.signalKill()
			err := <-ch
			s.clearProc()
			return err
		}
	default:
	}
	select {
	case <-s.stopCh:
		s.signalGraceful()
		select {
		case err := <-ch:
			s.clearProc()
			return err
		case <-time.After(s.cfg.stopGrace()):
			s.signalKill()
			err := <-ch
			s.clearProc()
			return err
		}
	case err := <-ch:
		s.clearProc()
		return err
	}
}

// killAndReap signals the child (SIGTERM → grace → SIGKILL) and reaps
// the Wait result. Only call when waitChild is not concurrently waiting
// (startOnce failure path, or run's defer after the loop exits).
func (s *launchSupervisor) killAndReap() {
	s.procMu.Lock()
	ch := s.waitDone
	cmd := s.cmd
	s.procMu.Unlock()
	if ch == nil {
		return
	}
	if cmd != nil && cmd.Process != nil {
		gracefulSignal(cmd.Process)
	}
	select {
	case <-ch:
		s.clearProc()
		return
	case <-time.After(s.cfg.stopGrace()):
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-ch
	s.clearProc()
}

func (s *launchSupervisor) signalGraceful() {
	s.procMu.Lock()
	cmd := s.cmd
	s.procMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		gracefulSignal(cmd.Process)
	}
}

func (s *launchSupervisor) signalKill() {
	s.procMu.Lock()
	cmd := s.cmd
	s.procMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (s *launchSupervisor) clearProc() {
	s.procMu.Lock()
	s.cmd = nil
	s.waitDone = nil
	s.procMu.Unlock()
}

func gracefulSignal(p *os.Process) {
	if p == nil {
		return
	}
	if runtime.GOOS == "windows" {
		// Windows has no SIGTERM equivalent that Go reliably delivers
		// to non-console children; Kill is the portable stop.
		_ = p.Kill()
		return
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		_ = p.Kill()
	}
}

// buildPluginEnv returns os.Environ plus MNEMO_PLUGIN_* variables.
func buildPluginEnv(name, home string, params map[string]any) []string {
	env := append([]string{}, os.Environ()...)
	env = append(env,
		"MNEMO_PLUGIN_NAME="+name,
		"MNEMO_PLUGIN_HOME="+home,
	)
	for k, v := range params {
		env = append(env, "MNEMO_PLUGIN_PARAM_"+paramEnvKey(k)+"="+stringifyParam(v))
	}
	return env
}

// paramEnvKey upper-cases k and maps '-' to '_' (grace-multiple → GRACE_MULTIPLE).
func paramEnvKey(k string) string {
	return strings.ToUpper(strings.ReplaceAll(k, "-", "_"))
}

func stringifyParam(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool, float64, float32, int, int32, int64, uint, uint32, uint64:
		return fmt.Sprint(x)
	case json.Number:
		return x.String()
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}
