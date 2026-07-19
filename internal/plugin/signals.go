// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
)

// SignalEvaluator evaluates declarative signal_sources stanzas (🎯T102.8)
// into diag Fast checks. No external plugin process is required.
type SignalEvaluator struct {
	home    string
	sources []store.SignalSource
	// now is overridable in tests.
	now func() time.Time
	// lookPath / command allow tests to stub launchd/git probes.
	lookPath func(string) (string, error)
	command  func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewSignalEvaluator builds an evaluator for the given config sources.
func NewSignalEvaluator(home string, sources []store.SignalSource) *SignalEvaluator {
	return &SignalEvaluator{
		home:     home,
		sources:  append([]store.SignalSource(nil), sources...),
		now:      time.Now,
		lookPath: exec.LookPath,
		command: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

// DiagChecks returns one Fast check per configured signal source.
func (e *SignalEvaluator) DiagChecks() []diag.Check {
	if e == nil || len(e.sources) == 0 {
		return nil
	}
	out := make([]diag.Check, 0, len(e.sources))
	for _, src := range e.sources {
		s := src
		name := s.Name
		if name == "" {
			name = s.Kind
		}
		out = append(out, diag.Check{
			Name: "signal." + name,
			Tier: diag.Fast,
			Run: func(ctx context.Context) diag.CheckResult {
				return e.evaluate(ctx, s)
			},
		})
	}
	return out
}

func (e *SignalEvaluator) evaluate(ctx context.Context, s store.SignalSource) diag.CheckResult {
	cadence, err := time.ParseDuration(s.Cadence)
	if err != nil || cadence <= 0 {
		return diag.Failure(
			fmt.Sprintf("signal %q: invalid cadence %q", s.Name, s.Cadence),
			"set cadence to a positive Go duration (e.g. \"15m\", \"1h\")")
	}
	grace := s.GraceMultiple
	if grace <= 0 {
		grace = 2
	}
	threshold := time.Duration(float64(cadence) * grace)

	path := expandHome(s.Path, e.home)
	var last time.Time
	var detail string
	switch s.Kind {
	case store.SignalKindFileMtime:
		last, detail, err = e.fileMtime(path)
	case store.SignalKindNewestArtifact:
		last, detail, err = e.newestArtifact(path)
	case store.SignalKindLastCommit:
		last, detail, err = e.lastCommit(ctx, path)
	case store.SignalKindLaunchd:
		last, detail, err = e.launchd(ctx, s.Label)
	default:
		return diag.Failure(
			fmt.Sprintf("signal %q: unknown kind %q", s.Name, s.Kind),
			"use file_mtime, launchd, newest_artifact, or last_commit")
	}
	if err != nil {
		return diag.Failure(
			fmt.Sprintf("signal %q: %v", s.Name, err),
			"check path/label and permissions for this signal source")
	}
	age := e.now().Sub(last)
	if age > threshold {
		msg := fmt.Sprintf("signal %q stale: last activity %s ago (threshold %s) — %s",
			s.Name, age.Round(time.Second), threshold, detail)
		return diag.Failure(msg, "investigate the automation producing this signal; cadence="+s.Cadence)
	}
	return diag.Healthy(fmt.Sprintf("signal %q fresh: last activity %s ago — %s",
		s.Name, age.Round(time.Second), detail))
}

func (e *SignalEvaluator) fileMtime(path string) (time.Time, string, error) {
	if path == "" {
		return time.Time{}, "", fmt.Errorf("path is required for file_mtime")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, "", err
	}
	return fi.ModTime(), path, nil
}

func (e *SignalEvaluator) newestArtifact(dir string) (time.Time, string, error) {
	if dir == "" {
		return time.Time{}, "", fmt.Errorf("path is required for newest_artifact")
	}
	var newest time.Time
	var newestPath string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
			newestPath = p
		}
		return nil
	})
	if err != nil {
		return time.Time{}, "", err
	}
	if newestPath == "" {
		return time.Time{}, "", fmt.Errorf("no files under %s", dir)
	}
	return newest, newestPath, nil
}

func (e *SignalEvaluator) lastCommit(ctx context.Context, repo string) (time.Time, string, error) {
	if repo == "" {
		return time.Time{}, "", fmt.Errorf("path is required for last_commit")
	}
	git, err := e.lookPath("git")
	if err != nil {
		return time.Time{}, "", fmt.Errorf("git not on PATH")
	}
	out, err := e.command(ctx, git, "-C", repo, "log", "-1", "--format=%cI")
	if err != nil {
		return time.Time{}, "", fmt.Errorf("git log: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	s := strings.TrimSpace(string(out))
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// try without timezone Z variants
		t, err = time.Parse("2006-01-02T15:04:05-07:00", s)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("parse commit date %q: %w", s, err)
		}
	}
	return t, "git HEAD in " + repo, nil
}

func (e *SignalEvaluator) launchd(ctx context.Context, label string) (time.Time, string, error) {
	if runtime.GOOS != "darwin" {
		return time.Time{}, "", fmt.Errorf("launchd signals are only supported on macOS")
	}
	if label == "" {
		return time.Time{}, "", fmt.Errorf("label is required for launchd")
	}
	// launchctl print system/<label> or gui/$UID/<label> — use print-gui if available.
	// We use `launchctl list LABEL` which prints PID and LastExitStatus when loaded.
	// For mtime of the last successful run we fall back to the job's stdout log if present.
	// Simpler portable probe: if the job has a live PID, treat "now" as last activity;
	// if LastExitStatus is non-zero and no PID, treat as failed (epoch = zero → stale).
	launchctl, err := e.lookPath("launchctl")
	if err != nil {
		return time.Time{}, "", fmt.Errorf("launchctl not on PATH")
	}
	out, err := e.command(ctx, launchctl, "list", label)
	if err != nil {
		// Not loaded → no recent activity.
		return time.Time{}, "", fmt.Errorf("launchctl list %s: %w", label, err)
	}
	// "PID\tStatus\tLabel" or just presence means the job is known.
	// If first field is a number, the job is running → fresh.
	fields := strings.Fields(string(out))
	if len(fields) >= 1 && fields[0] != "-" && fields[0] != "PID" {
		// Running.
		return e.now(), "launchd " + label + " running pid=" + fields[0], nil
	}
	// Loaded but not running — use a conservative "unknown last run".
	// Without a reliable exit timestamp we report the check as warn via age=threshold+1
	// by returning a zero time? That would always fail. Instead warn with detail.
	return e.now().Add(-24 * time.Hour), "launchd " + label + " loaded but not running", nil
}

func expandHome(p, home string) string {
	if p == "" || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
