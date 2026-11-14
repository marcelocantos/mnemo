// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Replication of staged snapshots to the destination (🎯T201).
//
// A staged snapshot is ~28 GB, the destination is a drive that can be
// unplugged at any moment, and the daemon can be restarted or the Mac
// rebooted halfway through. So the copy is incremental and resumable:
//
//   - It is written under a temporary name on the destination
//     (<name>.part), never under the final one.
//   - It goes in chunks of ReplicaChunk bytes. After each chunk the
//     destination file is fsynced and the chunk's SHA-256 is appended to
//     a progress file beside the staged snapshot, so the progress file
//     never claims more than is durably on the destination.
//   - A later pass, in this process or the next, re-verifies the last
//     recorded chunk against the destination and resumes after it.
//   - At the end every chunk of the destination file is read back and
//     compared with the hashes of the staged file, and the sizes are
//     compared. Only then is it fsynced, renamed into place, and the
//     staged copy deleted.
//
// Design decision: the replicator runs inside the daemon, not as a
// separate helper with its own code identity. The helper was weighed
// because macOS ties a removable-volume grant to the binary's code
// identity, and mnemo releases are ad-hoc signed (🎯T101 is set aside),
// so every upgrade is a new identity. A helper does not escape that: it
// would be rebuilt and re-signed by the same release, and Go embeds
// build and version information, so its identity would change with every
// release too unless it were frozen and built reproducibly across
// toolchain upgrades. And it would cover only half the problem: the
// daemon itself writes snapshots straight to `backup.dir` when the
// volume answers, so it needs the grant regardless. What fixes the grant
// is a stable signing identity for mnemo itself (🎯T101). Until then a
// missing grant costs a prompt after an upgrade, and in the meantime the
// snapshot is staged rather than lost.

// ReplicaChunk is how much is copied between fsyncs and progress
// records: the most a detach or crash can cost a resumed copy.
const ReplicaChunk = 64 << 20

// ReplicaPartExt is appended to a snapshot's name while it is being
// replicated onto the destination.
const ReplicaPartExt = ".part"

// replicaChunk is the chunk size in force; tests shrink it.
var replicaChunk int64 = ReplicaChunk

// replicaFault, when set, is called before each chunk is written; an
// error stands in for the destination failing mid-copy. Test seam.
var replicaFault func(name string, off int64) error

// isReplicaPart reports whether name is a replication in progress.
func isReplicaPart(name string) bool {
	if !strings.HasSuffix(name, ReplicaPartExt) {
		return false
	}
	_, _, ok := parseFilename(strings.TrimSuffix(name, ReplicaPartExt))
	return ok
}

// progressPath is where the progress of replicating staged lives.
func progressPath(staged string) string { return staged + ".progress.json" }

// replicaProgress is the content of a progress file.
type replicaProgress struct {
	Name    string   `json:"name"`
	Size    int64    `json:"size"`
	ModTime int64    `json:"mtime_ns"`
	Dest    string   `json:"dest"`
	Chunk   int64    `json:"chunk"`
	Hashes  []string `json:"sha256"` // one per chunk copied, verified and fsynced
}

// matches reports whether p describes the staged snapshot b.
func (p replicaProgress) matches(b Info) bool {
	return p.Name == b.Name && p.Size == b.Size && p.Chunk > 0
}

// verified is how many bytes p records as durably copied.
func (p replicaProgress) verified() int64 {
	return min(int64(len(p.Hashes))*p.Chunk, p.Size)
}

func readProgress(path string) (replicaProgress, bool) {
	var p replicaProgress
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &p) != nil {
		return replicaProgress{}, false
	}
	return p, true
}

func writeProgress(path string, p replicaProgress) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Replicator copies staged snapshots to the destination once it answers.
type Replicator struct {
	Dest       string    // the configured backup.dir
	StagingDir string    // where snapshots wait
	Retention  Retention // applied to Dest after each replication
	Poll       time.Duration
}

// replicaMu keeps two passes from copying at once.
var replicaMu sync.Mutex

// Run polls every Poll (default a minute) until ctx ends, replicating
// whatever is staged whenever the destination answers.
func (r *Replicator) Run(ctx context.Context) {
	poll := r.Poll
	if poll <= 0 {
		poll = time.Minute
	}
	for {
		if _, err := r.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("backup replicator: pass stopped", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// PassReport describes one replication pass.
type PassReport struct {
	Replicated []string // snapshots now on the destination, staged copy removed
	Superseded []string // staged snapshots retention would have discarded at once
	ResumedAt  int64    // offset the last copy resumed from (0 = from the start)
	Copied     int64    // bytes written to the destination in this pass
}

// Pass replicates every staged snapshot, oldest first, if the
// destination answers. A destination that is away is not an error: there
// is nothing to do until it returns.
func (r *Replicator) Pass(ctx context.Context) (PassReport, error) {
	var rep PassReport
	if !replicaMu.TryLock() {
		return rep, nil
	}
	defer replicaMu.Unlock()
	if r.StagingDir == "" || filepath.Clean(r.StagingDir) == filepath.Clean(r.Dest) {
		return rep, nil
	}
	staged, err := List(r.StagingDir)
	if err != nil {
		return rep, fmt.Errorf("list staging %s: %w", r.StagingDir, err)
	}
	if len(staged) == 0 {
		noteReplicated(r.StagingDir, "", time.Time{})
		return rep, nil
	}
	if err := EnsureDir(r.Dest); err != nil {
		return rep, nil // away; staged snapshots wait
	}
	r.removeStaleParts(staged)

	// Oldest first, so retention on the destination sees them in the
	// order they were taken.
	for i := len(staged) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		b := staged[i]
		if r.superseded(b) {
			r.dropStaged(b)
			rep.Superseded = append(rep.Superseded, b.Name)
			slog.Info("backup replicator: staged snapshot superseded by newer ones on the "+
				"destination; deleted without copying", "name", b.Name)
			continue
		}
		resumed, copied, err := r.replicate(ctx, b)
		rep.ResumedAt, rep.Copied = resumed, rep.Copied+copied
		if err != nil {
			return rep, fmt.Errorf("replicate %s: %w", b.Name, err)
		}
		rep.Replicated = append(rep.Replicated, b.Name)
	}
	if len(rep.Replicated) > 0 {
		if removed, err := GCOldest(r.Dest, r.Retention); err != nil {
			slog.Warn("backup replicator: retention on the destination failed", "dir", r.Dest, "err", err)
		} else if len(removed) > 0 {
			slog.Info("backup replicator: retention removed old snapshots", "count", len(removed))
		}
	}
	noteReplicated(r.StagingDir, lastOf(rep.Replicated), time.Now())
	return rep, nil
}

func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// removeStaleParts deletes partial copies on the destination whose staged
// source is gone (replaced by a newer staged snapshot, say).
func (r *Replicator) removeStaleParts(staged []Info) {
	want := map[string]bool{}
	for _, b := range staged {
		want[b.Name+ReplicaPartExt] = true
	}
	entries, err := readDir(r.Dest)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.isDir || !isReplicaPart(e.name) || want[e.name] {
			continue
		}
		if err := removeIn(r.Dest, filepath.Join(r.Dest, e.name)); err == nil {
			slog.Info("backup replicator: removed a partial copy whose staged source is gone",
				"name", e.name)
		}
	}
}

// superseded reports whether retention on the destination would delete b
// as soon as it arrived, because the destination already holds enough
// newer snapshots of b's pool. Copying 28 GB only to delete it is waste.
func (r *Replicator) superseded(b Info) bool {
	list, err := List(r.Dest)
	if err != nil {
		return false
	}
	keep := r.Retention.normalized()
	limit := keep.Keep
	if b.Tag == TagManual {
		limit = keep.KeepManual
	}
	newer := 0
	for _, d := range list {
		if d.Name == b.Name {
			continue
		}
		if (d.Tag == TagManual) == (b.Tag == TagManual) && d.Time.After(b.Time) {
			newer++
		}
	}
	return newer >= limit
}

// dropStaged deletes a staged snapshot and its progress record.
func (r *Replicator) dropStaged(b Info) {
	stageMu.Lock()
	defer stageMu.Unlock()
	if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
		slog.Warn("backup replicator: cannot remove staged snapshot", "path", b.Path, "err", err)
	}
	_ = os.Remove(progressPath(b.Path))
}

// replicate copies one staged snapshot to the destination, resuming any
// earlier progress. It returns the offset it resumed from and the bytes
// it wrote.
func (r *Replicator) replicate(ctx context.Context, b Info) (resumed, copied int64, err error) {
	final := filepath.Join(r.Dest, b.Name)
	part := final + ReplicaPartExt
	progPath := progressPath(b.Path)

	// Already there: a crash between the rename and deleting the staged
	// copy. The rename only happens after verification.
	if fi, err := statIn(r.Dest, final); err == nil {
		if fi.Size() == b.Size {
			slog.Info("backup replicator: snapshot already on the destination; removing staged copy",
				"name", b.Name)
			r.dropStaged(b)
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("%s already exists on the destination with a different size", b.Name)
	}

	src, err := os.Open(b.Path)
	if err != nil {
		return 0, 0, err
	}
	defer src.Close()
	sfi, err := src.Stat()
	if err != nil {
		return 0, 0, err
	}

	prog, ok := readProgress(progPath)
	if !ok || !prog.matches(b) || prog.ModTime != sfi.ModTime().UnixNano() ||
		prog.Dest != r.Dest || prog.Chunk != replicaChunk {
		prog = replicaProgress{Name: b.Name, Size: b.Size, ModTime: sfi.ModTime().UnixNano(),
			Dest: r.Dest, Chunk: replicaChunk}
	}

	dst, err := answer(r.Dest, "open", answerTimeout, func() (*os.File, error) {
		return os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	})
	if err != nil {
		return 0, 0, err
	}
	// Closing can block like any other call on the volume. The copy is
	// closed explicitly before its rename (Windows refuses to rename an
	// open file); this covers every earlier return.
	closed := false
	closeDst := func() error {
		if closed {
			return nil
		}
		closed = true
		_, err := answer(r.Dest, "close", answerTimeout, func() (struct{}, error) { return struct{}{}, dst.Close() })
		return err
	}
	defer closeDst()

	off, err := r.resumePoint(src, dst, &prog)
	if err != nil {
		return 0, 0, err
	}
	resumed = off
	if off > 0 {
		slog.Info("backup replicator: resuming", "name", b.Name,
			"from_gib", fmt.Sprintf("%.2f", gib(off)), "of_gib", fmt.Sprintf("%.2f", gib(b.Size)))
	} else {
		slog.Info("backup replicator: copying staged snapshot to the destination",
			"name", b.Name, "size_gib", fmt.Sprintf("%.2f", gib(b.Size)), "dest", r.Dest)
	}
	if err := writeProgress(progPath, prog); err != nil {
		return resumed, 0, fmt.Errorf("record progress: %w", err)
	}

	start := time.Now()
	// One buffer for the whole copy. A write that times out leaves its
	// goroutine holding the buffer, but the copy returns at once and a
	// later pass allocates its own.
	buf := make([]byte, replicaChunk)
	for off < b.Size {
		if err := ctx.Err(); err != nil {
			return resumed, copied, err
		}
		// A newer staged snapshot replaced this one; stop and let the
		// next pass tidy the partial copy away.
		if _, err := os.Stat(b.Path); err != nil {
			return resumed, copied, fmt.Errorf("staged snapshot went away mid-copy: %w", err)
		}
		n := min(replicaChunk, b.Size-off)
		chunk := buf[:n]
		if _, err := src.ReadAt(chunk, off); err != nil {
			return resumed, copied, fmt.Errorf("read staged: %w", err)
		}
		sum := sha256.Sum256(chunk)
		if replicaFault != nil {
			if err := replicaFault(b.Name, off); err != nil {
				return resumed, copied, unavailable(r.Dest, err)
			}
		}
		at := off
		if _, err := answer(r.Dest, "write", dataTimeout, func() (struct{}, error) {
			if _, err := dst.WriteAt(chunk, at); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, dst.Sync()
		}); err != nil {
			return resumed, copied, unavailable(r.Dest, err)
		}
		prog.Hashes = append(prog.Hashes, hex.EncodeToString(sum[:]))
		if err := writeProgress(progPath, prog); err != nil {
			return resumed, copied, fmt.Errorf("record progress: %w", err)
		}
		off += n
		copied += n
	}

	// Read the whole copy back and compare it, chunk by chunk, with the
	// hashes of the staged file.
	if err := r.verifyCopy(dst, prog); err != nil {
		// A copy that does not match is not resumable from any offset we
		// can trust: start again next pass.
		_ = os.Remove(progPath)
		_ = closeDst()
		_ = removeIn(r.Dest, part)
		return resumed, copied, err
	}
	if err := closeDst(); err != nil {
		return resumed, copied, unavailable(r.Dest, err)
	}
	if err := renameIn(r.Dest, part, final); err != nil {
		return resumed, copied, unavailable(r.Dest, err)
	}
	syncDir(r.Dest)
	// Closed before deleting: Windows will not delete an open file.
	src.Close()
	r.dropStaged(b)
	slog.Info("backup replicator: snapshot replicated; staged copy deleted",
		"name", b.Name, "dest", r.Dest, "copied_gib", fmt.Sprintf("%.2f", gib(copied)),
		"resumed_from_gib", fmt.Sprintf("%.2f", gib(resumed)), "elapsed", time.Since(start).Round(time.Second))
	return resumed, copied, nil
}

// resumePoint works out where a copy can safely continue from, and
// truncates the partial file there. The last recorded chunk is read back
// from the destination and checked against its hash; if it does not
// match, progress restarts from zero.
func (r *Replicator) resumePoint(src, dst *os.File, prog *replicaProgress) (int64, error) {
	fi, err := answer(r.Dest, "stat", answerTimeout, func() (os.FileInfo, error) { return dst.Stat() })
	if err != nil {
		return 0, err
	}
	have := fi.Size()
	k := len(prog.Hashes)
	for k > 0 && min(int64(k)*prog.Chunk, prog.Size) > have {
		k--
	}
	prog.Hashes = prog.Hashes[:k]
	if k > 0 {
		at := int64(k-1) * prog.Chunk
		n := min(prog.Chunk, prog.Size-at)
		got, err := answer(r.Dest, "read", dataTimeout, func() ([]byte, error) {
			buf := make([]byte, n)
			_, err := dst.ReadAt(buf, at)
			return buf, err
		})
		sum := sha256.Sum256(got)
		if err != nil || hex.EncodeToString(sum[:]) != prog.Hashes[k-1] {
			slog.Warn("backup replicator: partial copy does not match its record; starting again",
				"name", prog.Name, "err", err)
			prog.Hashes = nil
		}
	}
	off := prog.verified()
	if _, err := answer(r.Dest, "truncate", answerTimeout, func() (struct{}, error) {
		return struct{}{}, dst.Truncate(off)
	}); err != nil {
		return 0, err
	}
	return off, nil
}

// verifyCopy reads the destination file back and compares its size and
// every chunk's SHA-256 with the record taken from the staged file.
func (r *Replicator) verifyCopy(dst *os.File, prog replicaProgress) error {
	fi, err := answer(r.Dest, "stat", answerTimeout, func() (os.FileInfo, error) { return dst.Stat() })
	if err != nil {
		return err
	}
	if fi.Size() != prog.Size {
		return fmt.Errorf("verify: copy is %d bytes, staged snapshot is %d", fi.Size(), prog.Size)
	}
	want := int((prog.Size + prog.Chunk - 1) / prog.Chunk)
	if len(prog.Hashes) != want {
		return fmt.Errorf("verify: %d chunk hashes recorded, want %d", len(prog.Hashes), want)
	}
	for i, h := range prog.Hashes {
		at := int64(i) * prog.Chunk
		n := min(prog.Chunk, prog.Size-at)
		sum, err := answer(r.Dest, "read", dataTimeout, func() ([32]byte, error) {
			hw := sha256.New()
			if _, err := io.Copy(hw, io.NewSectionReader(dst, at, n)); err != nil {
				return [32]byte{}, err
			}
			var s [32]byte
			copy(s[:], hw.Sum(nil))
			return s, nil
		})
		if err != nil {
			return unavailable(r.Dest, err)
		}
		want, _ := hex.DecodeString(h)
		if !bytes.Equal(sum[:], want) {
			return fmt.Errorf("verify: chunk %d of %s differs from the staged snapshot", i, prog.Name)
		}
	}
	return nil
}

// syncDir fsyncs a directory so a rename into it is durable. Best effort:
// Windows cannot open a directory for syncing, and NTFS journals renames.
func syncDir(dir string) {
	_, _ = answer(dir, "fsync", answerTimeout, func() (struct{}, error) {
		f, err := os.Open(dir)
		if err != nil {
			return struct{}{}, err
		}
		defer f.Close()
		return struct{}{}, f.Sync()
	})
}
