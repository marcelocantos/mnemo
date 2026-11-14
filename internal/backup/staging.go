// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Staging (🎯T201).
//
// A custom `backup.dir` usually sits on an external volume because the
// database's own volume has no room for a second copy of the store. That
// volume is sometimes away: unplugged, or, after an upgrade, withheld by
// macOS until someone answers a removable-volume prompt. 🎯T199 made
// that a recorded skip, and a skip costs the night's backup.
//
// Staging trades a bounded amount of space on the database volume for
// never losing a snapshot to an absent drive. When the destination does
// not answer, the snapshot is written to the staging directory instead,
// and the replicator (replicate.go) copies it to the destination once
// that answers again, then deletes the staged copy. Staging holds at most
// one snapshot per tag: a newer staged snapshot replaces an older one
// still waiting, so a long absence costs one snapshot's worth of space
// per tag, not one per night.

// StagedMaxAge is how long snapshots may wait in staging before /health
// warns that the newest backup is still on the same disk as the
// database. A week covers a laptop away from its desk for a few days; past
// that a disk failure would take the store and its only backup together.
const StagedMaxAge = 7 * 24 * time.Hour

// StagingSubdir is the default staging directory's name inside
// DefaultDir.
const StagingSubdir = "staging"

// DefaultStagingDir is the staging directory beside the database at
// dbPath: <db dir>/backups/staging.
func DefaultStagingDir(dbPath string) string {
	return filepath.Join(DefaultDir(dbPath), StagingSubdir)
}

// stagingStateFile, in the staging directory, records since when
// snapshots have been waiting there, and the last one replicated.
const stagingStateFile = ".staging.json"

// Placement is where a snapshot bound for a destination is written.
type Placement struct {
	Dir    string // the destination, or the staging directory
	Staged bool   // Dir is the staging directory because the destination was away
	Cause  error  // why the destination could not be used, when Staged
}

// Place decides where a snapshot for dest is written: dest itself when
// it answers (🎯T200's bounded wait), otherwise stagingDir.
//
// An empty stagingDir, or one equal to dest, disables staging, and an
// unusable dest is then returned as ErrDestinationUnavailable as before
// (🎯T199). So is a staging directory that is not usable either; the
// error names both.
func Place(dest, stagingDir string) (Placement, error) {
	err := EnsureDir(dest)
	if err == nil {
		return Placement{Dir: dest}, nil
	}
	if stagingDir == "" || filepath.Clean(stagingDir) == filepath.Clean(dest) {
		return Placement{}, err
	}
	if serr := EnsureDir(stagingDir); serr != nil {
		return Placement{}, fmt.Errorf("%w; the staging directory is not usable either: %v", err, serr)
	}
	return Placement{Dir: stagingDir, Staged: true, Cause: err}, nil
}

// stageMu serialises changes to the staging directory's bookkeeping:
// pruning, and the state file.
var stageMu sync.Mutex

// stagingState is the content of stagingStateFile.
type stagingState struct {
	// WaitingSince is when the oldest snapshot still waiting was staged,
	// counted from the moment staging last went from empty to non-empty.
	// A newer staged snapshot replacing an older one does not reset it:
	// what it measures is how long the destination has gone without the
	// newest backup.
	WaitingSince time.Time `json:"waiting_since,omitzero"`
	// LastReplicated is the most recent snapshot the replicator copied
	// to the destination, and when.
	LastReplicated   string    `json:"last_replicated,omitempty"`
	LastReplicatedAt time.Time `json:"last_replicated_at,omitzero"`
}

func readStagingState(stagingDir string) stagingState {
	var st stagingState
	b, err := os.ReadFile(filepath.Join(stagingDir, stagingStateFile))
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func writeStagingState(stagingDir string, st stagingState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(stagingDir, stagingStateFile+".new")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stagingDir, stagingStateFile))
}

// RetainPlaced applies retention after a snapshot was written to pl.Dir.
//
// A snapshot written to the destination prunes the destination exactly as
// before (GCOldest). A staged one replaces any older staged snapshot of
// the same tag — staging holds at most one per tag — and starts the
// waiting clock if staging was empty. Returns the paths removed.
func RetainPlaced(pl Placement, tag Tag, written string, r Retention) ([]string, error) {
	if !pl.Staged {
		return GCOldest(pl.Dir, r)
	}
	stageMu.Lock()
	defer stageMu.Unlock()
	list, err := List(pl.Dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, b := range list {
		if b.Tag != tag || b.Path == written {
			continue
		}
		if err := os.Remove(b.Path); err != nil {
			slog.Warn("backup staging: cannot remove superseded staged snapshot",
				"path", b.Path, "err", err)
			continue
		}
		_ = os.Remove(progressPath(b.Path))
		removed = append(removed, b.Path)
		slog.Info("backup staging: newer snapshot replaces one still waiting for the destination",
			"removed", b.Name, "kept", filepath.Base(written))
	}
	st := readStagingState(pl.Dir)
	if st.WaitingSince.IsZero() {
		st.WaitingSince = time.Now()
		if _, at, ok := parseFilename(filepath.Base(written)); ok {
			st.WaitingSince = at
		}
		if err := writeStagingState(pl.Dir, st); err != nil {
			slog.Warn("backup staging: cannot record staging state", "dir", pl.Dir, "err", err)
		}
	}
	return removed, nil
}

// StagedSnapshot is a snapshot waiting in staging.
type StagedSnapshot struct {
	Info
	Copied int64 // bytes already copied to the destination and verified (resumable)
}

// StagingView is what is waiting in the staging directory, for
// backup_status and /health.
type StagingView struct {
	Dir              string
	Staged           []StagedSnapshot // newest first
	WaitingSince     time.Time        // zero when nothing is waiting
	LastReplicated   string
	LastReplicatedAt time.Time
}

// Staging reads the staging directory. An empty stagingDir (staging
// disabled) or a missing directory is an empty view.
func Staging(stagingDir string) (StagingView, error) {
	v := StagingView{Dir: stagingDir}
	if stagingDir == "" {
		return v, nil
	}
	list, err := List(stagingDir)
	if err != nil {
		return v, err
	}
	st := readStagingState(stagingDir)
	v.LastReplicated, v.LastReplicatedAt = st.LastReplicated, st.LastReplicatedAt
	for _, b := range list {
		s := StagedSnapshot{Info: b}
		if p, ok := readProgress(progressPath(b.Path)); ok && p.matches(b) {
			s.Copied = p.verified()
		}
		v.Staged = append(v.Staged, s)
	}
	if len(v.Staged) > 0 {
		v.WaitingSince = st.WaitingSince
		// A state file lost or never written: the oldest staged snapshot
		// has been waiting at least since it was taken.
		oldest := v.Staged[len(v.Staged)-1].Time
		if v.WaitingSince.IsZero() || oldest.Before(v.WaitingSince) {
			v.WaitingSince = oldest
		}
	}
	return v, nil
}

// StagingVerdict is the three-way state backup_status and backup.recent
// report (🎯T201).
type StagingVerdict int

const (
	// NothingStaged: snapshots go to the destination and none is waiting.
	NothingStaged StagingVerdict = iota
	// StagedWaiting: a snapshot is staged on the database volume and
	// waiting for the destination. Healthy while it is fresh.
	StagedWaiting
	// StagedTooLong: snapshots have waited longer than StagedMaxAge; the
	// newest backup is still on the same disk as the database.
	StagedTooLong
)

// Verdict classifies v at now.
func (v StagingView) Verdict(now time.Time) StagingVerdict {
	switch {
	case len(v.Staged) == 0:
		return NothingStaged
	case now.Sub(v.WaitingSince) > StagedMaxAge:
		return StagedTooLong
	default:
		return StagedWaiting
	}
}

// Summary describes what is staged in one line, or "" when nothing is.
func (v StagingView) Summary(now time.Time) string {
	if len(v.Staged) == 0 {
		return ""
	}
	var names []string
	var copying string
	for _, s := range v.Staged {
		names = append(names, s.Name)
		if s.Copied > 0 && copying == "" {
			copying = fmt.Sprintf("; replicating %s: %.1f of %.1f GiB copied (%d%%)",
				s.Name, gib(s.Copied), gib(s.Size), s.Copied*100/max(s.Size, 1))
		}
	}
	return fmt.Sprintf("%d snapshot(s) staged on the database volume in %s, waiting for the "+
		"destination for %s (%s)%s", len(v.Staged), v.Dir,
		now.Sub(v.WaitingSince).Round(time.Minute), strings.Join(names, ", "), copying)
}

// noteReplicated records the last snapshot replicated (name may be
// empty), and resets the waiting clock once staging holds no snapshot.
func noteReplicated(stagingDir, name string, at time.Time) {
	stageMu.Lock()
	defer stageMu.Unlock()
	st := readStagingState(stagingDir)
	if name != "" {
		st.LastReplicated, st.LastReplicatedAt = name, at
	}
	if list, err := List(stagingDir); err == nil && len(list) == 0 {
		st.WaitingSince = time.Time{}
	}
	if err := writeStagingState(stagingDir, st); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("backup staging: cannot record staging state", "dir", stagingDir, "err", err)
	}
}
