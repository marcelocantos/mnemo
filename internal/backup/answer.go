// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Bounded waits on a backup destination (🎯T200).
//
// On 2026-11-13 two releases in a row wedged on a single open() of
// /Volumes/Storage/mnemo/backups. The call did not fail; it never
// returned. The likely cause is macOS holding it while it waits on a
// removable-volume privacy decision for a newly signed binary. v0.116.0's
// worker went silent at 03:16, and v0.117.0 never finished bringing its
// store up, because the startup orphan sweep ran on that path.
//
// So every file-system call that touches a backup directory goes through
// answer: the call runs on its own goroutine, and a caller that has
// waited AnswerTimeout gets ErrDestinationUnavailable and carries on. The
// stranded goroutine is not abandoned blindly. While it is still inside
// the kernel, its directory counts as not answering, and further calls
// against it fail at once rather than stranding a thread each. A health
// check polled every few seconds would otherwise pile up blocked OS
// threads until the runtime hit its limit.

// AnswerTimeout bounds a metadata call (stat, readdir, mkdir, remove,
// rename, a small file) on a backup directory.
//
// A healthy volume answers in milliseconds, and a spun-down USB disk
// within a second or two. Five seconds rules out neither, and is short
// enough that a tool call waiting behind it still answers.
const AnswerTimeout = 5 * time.Second

// DataTimeout bounds one chunk of data I/O (a write of ReplicaChunk
// bytes, or an fsync) on a backup directory. It is longer than
// AnswerTimeout because a 64 MiB write to a slow USB disk legitimately
// takes over a second.
const DataTimeout = 60 * time.Second

// answerTimeout and dataTimeout are the values in force; tests shorten
// them.
var (
	answerTimeout = AnswerTimeout
	dataTimeout   = DataTimeout
)

// hangHook, when set, runs inside the bounded call before fn: a test
// blocks in it to stand in for a destination whose stat, readdir or open
// never returns.
var hangHook func(dir, op string)

// pending records, per directory, the call still stuck inside the
// kernel after its caller gave up on it.
var (
	pendingMu sync.Mutex
	pending   = map[string]pendingCall{}
)

type pendingCall struct {
	op    string
	since time.Time
}

// answer runs fn, which touches dir, and waits at most timeout for it.
//
// A call that has not returned in time leaves dir marked as not
// answering until it does return; meanwhile every other call on dir is
// refused immediately with the same error. fn must not share mutable
// state with its caller, since it may still be running after answer has
// returned: hand results back through its return value.
func answer[T any](dir, op string, timeout time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	key := filepath.Clean(dir)
	pendingMu.Lock()
	if p, stuck := pending[key]; stuck {
		pendingMu.Unlock()
		return zero, notAnswering(dir, p.op, time.Since(p.since))
	}
	pendingMu.Unlock()

	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		if hangHook != nil {
			hangHook(key, op)
		}
		v, err := fn()
		pendingMu.Lock()
		if p, ok := pending[key]; ok && p.since.Equal(start) {
			delete(pending, key)
		}
		pendingMu.Unlock()
		done <- result{v, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
	}
	pendingMu.Lock()
	select {
	case r := <-done:
		// Returned while the lock was being taken.
		pendingMu.Unlock()
		return r.v, r.err
	default:
	}
	if _, stuck := pending[key]; !stuck {
		pending[key] = pendingCall{op: op, since: start}
	}
	pendingMu.Unlock()
	return zero, notAnswering(dir, op, timeout)
}

// notAnswering is the error for a call on dir that did not return.
func notAnswering(dir, op string, waited time.Duration) error {
	return fmt.Errorf("%w: %s (%s has not answered after %s: macOS may be waiting on a "+
		"removable-volume permission decision for this mnemo binary, or the volume is unresponsive)",
		ErrDestinationUnavailable, dir, op, waited.Round(time.Second))
}

// NotAnswering reports whether a call on dir is still stuck, and since
// when. It does not touch the file system.
func NotAnswering(dir string) (op string, since time.Time, stuck bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pending[filepath.Clean(dir)]
	return p.op, p.since, ok
}

// fileEntry is what a guarded directory listing returns: the fields of
// os.DirEntry and its os.FileInfo that the package uses, gathered inside
// the bounded call. DirEntry.Info is itself an lstat, so reading it
// outside the call would be one more unguarded system call.
type fileEntry struct {
	name    string
	isDir   bool
	size    int64
	modTime time.Time
	infoErr error
}

// readDir lists dir under a bounded wait. A directory that does not
// exist is empty, not an error.
func readDir(dir string) ([]fileEntry, error) {
	return answer(dir, "readdir", answerTimeout, func() ([]fileEntry, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		out := make([]fileEntry, 0, len(entries))
		for _, e := range entries {
			fe := fileEntry{name: e.Name(), isDir: e.IsDir()}
			if fi, err := e.Info(); err != nil {
				fe.infoErr = err
			} else {
				fe.size, fe.modTime = fi.Size(), fi.ModTime()
			}
			out = append(out, fe)
		}
		return out, nil
	})
}

// statIn stats path, which lives under dir, under a bounded wait.
func statIn(dir, path string) (os.FileInfo, error) {
	return answer(dir, "stat", answerTimeout, func() (os.FileInfo, error) { return os.Stat(path) })
}

// removeIn removes path, which lives under dir, under a bounded wait.
func removeIn(dir, path string) error {
	_, err := answer(dir, "remove", answerTimeout, func() (struct{}, error) {
		return struct{}{}, os.Remove(path)
	})
	return err
}

// renameIn renames within dir under a bounded wait.
func renameIn(dir, from, to string) error {
	_, err := answer(dir, "rename", answerTimeout, func() (struct{}, error) {
		return struct{}{}, os.Rename(from, to)
	})
	return err
}

// readFileIn reads a small file under dir under a bounded wait.
func readFileIn(dir, path string) ([]byte, error) {
	return answer(dir, "read", answerTimeout, func() ([]byte, error) { return os.ReadFile(path) })
}

// probe opens dir and reads one name from it, under a bounded wait. It is
// the call macOS was seen to hold: open() on the directory. A stat can
// succeed from cached metadata where an open does not.
func probe(dir string) error {
	_, err := answer(dir, "open", answerTimeout, func() (struct{}, error) {
		f, err := os.Open(dir)
		if err != nil {
			return struct{}{}, err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return struct{}{}, err
		}
		if !fi.IsDir() {
			return struct{}{}, errNotDir
		}
		if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}

var errNotDir = errors.New("not a directory")
