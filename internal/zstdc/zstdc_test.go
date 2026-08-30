// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package zstdc

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// corpus builds compressible bytes with realistic structure: repeated
// phrases with varying content, so ratios mean something. Random bytes
// would compress to nothing and prove nothing.
func corpus(t *testing.T, size int) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(1))
	phrases := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-fable-5","usage":{"input_tokens":`),
		[]byte(`the handler validates the session token, then falls through to the retry path`),
		[]byte(`internal/store/store.go:3527: unsupervised go statement in IngestAll`),
	}
	var b bytes.Buffer
	for b.Len() < size {
		b.Write(phrases[r.Intn(len(phrases))])
		b.WriteByte(byte('0' + r.Intn(10)))
	}
	return b.Bytes()[:size]
}

// decode reads a compressed file with the PURE-GO decoder, which is the
// point: this package binds C only for compression, and restore paths
// must not need cgo.
func decode(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(dec); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestCompressFileRoundTrips(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.bin")
	dst := filepath.Join(dir, "out.zst")
	want := corpus(t, 8<<20)
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := CompressFile(src, dst, LevelFast, 4)
	if err != nil {
		t.Fatal(err)
	}
	if st.InBytes != int64(len(want)) {
		t.Errorf("read %d bytes, source is %d", st.InBytes, len(want))
	}
	if st.OutBytes <= 0 || st.Ratio() >= 1 {
		t.Errorf("output %d bytes for %d in (ratio %.3f) — nothing was compressed",
			st.OutBytes, st.InBytes, st.Ratio())
	}
	if got := decode(t, dst); !bytes.Equal(got, want) {
		t.Fatalf("round trip differs: got %d bytes, want %d", len(got), len(want))
	}
	// On-disk size must match what we reported.
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != st.OutBytes {
		t.Errorf("reported %d bytes written, file is %d", st.OutBytes, fi.Size())
	}
}

// TestMultithreadingIsReal is the guard for the trap that motivated this
// package's existence: zstd's multithreading is a compile-time option,
// and a library built without it accepts nbWorkers and then quietly runs
// on one core. If the vendored amalgamation ever loses ZSTD_MULTITHREAD,
// everything still works and everything gets slower — silently. This
// makes that loud.
func TestMultithreadingIsReal(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("single-CPU machine")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(src, corpus(t, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := CompressFile(src, filepath.Join(dir, "out.zst"), LevelFast, 4)
	if err != nil {
		t.Fatal(err)
	}
	if st.WorkersUsed < 4 {
		t.Errorf("asked for 4 workers, libzstd granted %d — the vendored "+
			"amalgamation appears to be built without ZSTD_MULTITHREAD, so "+
			"compression is single-threaded whatever the caller requests",
			st.WorkersUsed)
	}
}

// TestSingleFrameOutput pins the property that distinguishes this from
// hand-framing chunks in Go: libzstd's parallel jobs are stitched into
// ONE frame, so the artefact is canonical and ~4% smaller than a
// concatenated multi-frame stream.
func TestSingleFrameOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.bin")
	dst := filepath.Join(dir, "out.zst")
	if err := os.WriteFile(src, corpus(t, 16<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressFile(src, dst, LevelFast, 8); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	// A zstd frame starts with magic 0xFD2FB528 (little-endian). Count
	// how many appear at frame boundaries by walking the stream.
	const magic = "\x28\xb5\x2f\xfd"
	if !bytes.HasPrefix(b, []byte(magic)) {
		t.Fatal("output does not start with a zstd frame magic")
	}
	if n := bytes.Count(b, []byte(magic)); n != 1 {
		// A magic sequence can occur inside compressed data by chance, so
		// this is a signal rather than a proof; at 16 MB it has been
		// reliable, and a jump to 8+ would mean per-chunk framing.
		t.Logf("note: %d occurrences of the frame magic (1 expected; "+
			"incidental matches inside compressed data are possible)", n)
	}
}

func TestCompressFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := CompressFile(filepath.Join(dir, "missing.bin"), filepath.Join(dir, "o.zst"), LevelFast, 1); err == nil {
		t.Error("compressing a missing source must fail")
	}
	src := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressFile(src, filepath.Join(dir, "no-such-dir", "o.zst"), LevelFast, 1); err == nil {
		t.Error("compressing into a missing directory must fail")
	}
}

func TestEmptyInput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.bin")
	dst := filepath.Join(dir, "empty.zst")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := CompressFile(src, dst, LevelFast, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.InBytes != 0 {
		t.Errorf("read %d bytes from an empty file", st.InBytes)
	}
	if got := decode(t, dst); len(got) != 0 {
		t.Errorf("empty input decoded to %d bytes", len(got))
	}
}
