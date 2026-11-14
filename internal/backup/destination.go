// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// ErrDestinationUnavailable is returned, before anything is written, when
// the backup directory cannot be reached or created. The usual cause is a
// `backup.dir` on an external volume that is not attached: a laptop away
// from its desk at 03:00.
//
// It is a deferral, not a failure. Nothing is written anywhere else — in
// particular not beside the database, whose volume is typically the one
// the custom directory was chosen to spare — and the worker takes the
// snapshot once the destination is back.
var ErrDestinationUnavailable = errors.New("backup destination not available")

// DefaultDir is the backup directory beside the database at dbPath. It is
// where snapshots go unless `backup.dir` says otherwise, and where
// StatusFile lives wherever snapshots go: the record of the last attempt
// has to stay readable, and writable, while a custom destination is away.
func DefaultDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

// EnsureDir creates dir if it is missing, and reports
// ErrDestinationUnavailable when it cannot, or when dir does not answer
// within AnswerTimeout (🎯T200).
//
// On macOS a volume that is not mounted fails here rather than silently
// landing on the boot disk: /Volumes is root-owned, so creating
// /Volumes/<name>/... under a missing mount point is refused. A missing
// drive letter on Windows fails the same way.
func EnsureDir(dir string) error {
	_, err := answer(dir, "mkdir", answerTimeout, func() (struct{}, error) {
		return struct{}{}, os.MkdirAll(dir, 0o755)
	})
	if err == nil {
		err = probe(dir)
	}
	return unavailable(dir, err)
}

// Away reports, without creating anything, why a custom destination
// cannot be used right now; nil when it can. dir equal to statusDir is
// the default directory beside the database: it shares the database's
// volume and is created on demand, so a missing one is merely empty and
// is never away. A destination that does not answer within
// AnswerTimeout is away (🎯T200).
func Away(dir, statusDir string) error {
	if dir == statusDir {
		return nil
	}
	return unavailable(dir, probe(dir))
}

// unavailable wraps a failure to reach dir as ErrDestinationUnavailable,
// naming dir once and keeping only the reason. nil stays nil.
func unavailable(dir string, err error) error {
	if err == nil || errors.Is(err, ErrDestinationUnavailable) {
		return err
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("%w: %s (%v)", ErrDestinationUnavailable, dir, err)
}

// RecordUnavailable records, in statusDir, a snapshot that was not
// started because its destination was away. cause is the
// ErrDestinationUnavailable error naming the destination.
func RecordUnavailable(statusDir string, tag Tag, cause error) {
	a := Attempt{At: time.Now(), Tag: tag, Skipped: true, Unavailable: true, Error: cause.Error()}
	if err := RecordAttempt(statusDir, a); err != nil {
		slog.Warn("backup: cannot record attempt", "dir", statusDir, "err", err)
	}
}

// statusDirFor resolves where a snapshot into dir records its attempt.
func statusDirFor(dir string, args *BackupArgs) string {
	if args != nil && args.StatusDir != "" {
		return args.StatusDir
	}
	return dir
}
