// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"fmt"
	"os"
)

// ErrInsufficientSpace is returned, before anything is written, when the
// backup volume cannot hold a snapshot.
var ErrInsufficientSpace = errors.New("insufficient free space for backup")

// spaceFactor and spaceMargin size the free-space precheck. A snapshot
// holds two files at its peak: the VACUUM INTO copy (at most the size
// of the database plus its WAL) and the zstd output being written from
// it. On 19 Oct 2026 a 43 GB store peaked at 42.2 GB raw + 23.8 GB
// compressed, 1.56x the raw size. 1.75x plus 1 GiB covers that with
// room for a less compressible store.
const (
	spaceFactor = 1.75
	spaceMargin = 1 << 30
)

// freeBytesFn reports free bytes on the volume holding dir. A var so
// tests can simulate a full disk.
var freeBytesFn = freeBytes

// RequiredSpace is the free space a snapshot of srcPath needs.
func RequiredSpace(srcPath string) (int64, error) {
	st, err := os.Stat(srcPath)
	if err != nil {
		return 0, err
	}
	n := st.Size()
	if wal, err := os.Stat(srcPath + "-wal"); err == nil {
		n += wal.Size()
	}
	return int64(float64(n)*spaceFactor) + spaceMargin, nil
}

// checkSpace fails with ErrInsufficientSpace when dir's volume cannot
// hold a snapshot of srcPath. A volume whose free space cannot be read
// is not refused: the precheck exists to avoid a doomed run, not to add
// a new way for backups to stop.
func checkSpace(srcPath, dir string) error {
	need, err := RequiredSpace(srcPath)
	if err != nil {
		return fmt.Errorf("size source database: %w", err)
	}
	free, err := freeBytesFn(dir)
	if err != nil {
		return nil
	}
	if free < need {
		return fmt.Errorf("%w: %.1f GiB free in %s, need %.1f GiB (%.2fx the database plus 1 GiB)",
			ErrInsufficientSpace, gib(free), dir, gib(need), spaceFactor)
	}
	return nil
}

// FreeBytes reports free bytes on the volume holding dir.
func FreeBytes(dir string) (int64, error) { return freeBytesFn(dir) }
