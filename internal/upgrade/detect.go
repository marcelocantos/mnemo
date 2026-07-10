// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultReleaseRepo is the GitHub repo queried for latest tags.
const DefaultReleaseRepo = "marcelocantos/mnemo"

// TagFetcher returns the latest release tag string (e.g. "v0.62.0").
// Tests inject fakes; production uses GHReleaseFetcher.
type TagFetcher func(ctx context.Context) (string, error)

// GHReleaseFetcher runs `gh release list` for repo and returns the
// newest tag. repo defaults to DefaultReleaseRepo when empty.
func GHReleaseFetcher(repo string) TagFetcher {
	if repo == "" {
		repo = DefaultReleaseRepo
	}
	return func(ctx context.Context) (string, error) {
		cmd := exec.CommandContext(ctx, "gh", "release", "list",
			"--repo", repo,
			"--limit", "1",
			"--json", "tagName")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return "", fmt.Errorf("gh release list: %s", msg)
		}
		var rows []struct {
			TagName string `json:"tagName"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
			return "", fmt.Errorf("parse gh release list: %w", err)
		}
		if len(rows) == 0 || rows[0].TagName == "" {
			return "", fmt.Errorf("no releases listed for %s", repo)
		}
		return rows[0].TagName, nil
	}
}

// DetectorArgs configures a Detector.
type DetectorArgs struct {
	// CurrentVersion is the running binary version (no required "v" prefix).
	CurrentVersion string
	// Fetch retrieves the latest tag. Required when Disabled is false.
	Fetch TagFetcher
	// Disabled skips all network calls (config disable_upgrade_check).
	Disabled bool
	// MinInterval bounds how often Check will call Fetch (default 6h).
	MinInterval time.Duration
	// Now is optional clock injection for tests.
	Now func() time.Time
}

// Detector caches the latest known release tag and compares it to the
// running binary version (🎯T97.2).
type Detector struct {
	mu             sync.Mutex
	current        string
	fetch          TagFetcher
	disabled       bool
	minInterval    time.Duration
	now            func() time.Time
	latest         string
	checkedAt      time.Time
	lastErr        string
	fetchCount     int // for tests: how many times Fetch was invoked
	upgradeAvail   bool
}

// NewDetector builds a Detector from args.
func NewDetector(args *DetectorArgs) *Detector {
	if args == nil {
		args = &DetectorArgs{}
	}
	min := args.MinInterval
	if min <= 0 {
		min = 6 * time.Hour
	}
	now := args.Now
	if now == nil {
		now = time.Now
	}
	return &Detector{
		current:     args.CurrentVersion,
		fetch:       args.Fetch,
		disabled:    args.Disabled,
		minInterval: min,
		now:         now,
	}
}

// SetDisabled updates the disable flag (config hot-reload).
func (d *Detector) SetDisabled(disabled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disabled = disabled
}

// FetchCount returns how many times the underlying TagFetcher ran.
func (d *Detector) FetchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fetchCount
}

// Snapshot is a point-in-time view of detection state for diagnostics.
type Snapshot struct {
	Current        string
	Latest         string
	CheckedAt      time.Time
	Disabled       bool
	UpgradeAvail   bool
	LastError      string
	FetchCount     int
}

// Snapshot returns the current detection state.
func (d *Detector) Snapshot() Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Snapshot{
		Current:      d.current,
		Latest:       d.latest,
		CheckedAt:    d.checkedAt,
		Disabled:     d.disabled,
		UpgradeAvail: d.upgradeAvail,
		LastError:    d.lastErr,
		FetchCount:   d.fetchCount,
	}
}

// CheckResult is the outcome of one detection pass.
type CheckResult struct {
	// CalledNetwork is true only when Fetch was invoked this call.
	CalledNetwork bool
	// UpgradeAvailable is true when latest > current.
	UpgradeAvailable bool
	// Latest is the newest tag when known.
	Latest string
	// Detail is a human-readable summary for diag.
	Detail string
	// Err is set when a fetch was attempted and failed.
	Err error
}

// Check consults the cache / fetcher. When disabled, never calls Fetch.
func (d *Detector) Check(ctx context.Context) CheckResult {
	d.mu.Lock()
	disabled := d.disabled
	current := d.current
	fetch := d.fetch
	minInterval := d.minInterval
	now := d.now()
	lastChecked := d.checkedAt
	cachedLatest := d.latest
	d.mu.Unlock()

	if disabled {
		return CheckResult{
			CalledNetwork:    false,
			UpgradeAvailable: false,
			Detail:           "upgrade check disabled (disable_upgrade_check)",
		}
	}
	if fetch == nil {
		return CheckResult{
			Detail: "upgrade check has no tag fetcher configured",
			Err:    fmt.Errorf("nil TagFetcher"),
		}
	}

	needFetch := lastChecked.IsZero() || now.Sub(lastChecked) >= minInterval || cachedLatest == ""
	var latest string
	var called bool
	var fetchErr error
	if needFetch {
		called = true
		latest, fetchErr = fetch(ctx)
		d.mu.Lock()
		d.fetchCount++
		d.checkedAt = now
		if fetchErr != nil {
			d.lastErr = fetchErr.Error()
		} else {
			d.latest = latest
			d.lastErr = ""
		}
		d.mu.Unlock()
		if fetchErr != nil {
			return CheckResult{
				CalledNetwork: true,
				Detail:        "failed to query latest release: " + fetchErr.Error(),
				Err:           fetchErr,
			}
		}
	} else {
		latest = cachedLatest
	}

	newer, err := IsNewer(current, latest)
	if err != nil {
		return CheckResult{
			CalledNetwork: called,
			Latest:        latest,
			Detail:        "version compare failed: " + err.Error(),
			Err:           err,
		}
	}
	d.mu.Lock()
	d.upgradeAvail = newer
	d.latest = latest
	d.mu.Unlock()

	if newer {
		return CheckResult{
			CalledNetwork:    called,
			UpgradeAvailable: true,
			Latest:           latest,
			Detail: fmt.Sprintf("upgrade available: running %s, latest %s",
				formatV(current), formatV(latest)),
		}
	}
	return CheckResult{
		CalledNetwork: called,
		Latest:        latest,
		Detail: fmt.Sprintf("up to date: running %s (latest %s)",
			formatV(current), formatV(latest)),
	}
}

func formatV(v string) string {
	n := NormalizeTag(v)
	if n == "" {
		return v
	}
	return "v" + n
}
