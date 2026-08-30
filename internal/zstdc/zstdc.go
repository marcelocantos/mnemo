// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package zstdc compresses a file with the vendored C libzstd, using its
// multithreaded encoder (🎯T159).
//
// mnemo already carries a pure-Go zstd (klauspost/compress, which
// implements the per-row compression of 🎯T151), and that stays the right
// tool for small buffers. This package exists for the one case it cannot
// serve: compressing a multi-gigabyte backup quickly, at the ratio the
// reference implementation achieves.
//
// Measured on a slice of an 18.9 GB index, 16 cores:
//
//	gzip -1 (what backups used)  ratio 0.734  18.1s per 1.5 GB
//	klauspost, streaming         ratio 0.707  parallel ceiling ~1.4x
//	klauspost, hand-framed       ratio 0.732  full parallelism, but a
//	                                          multi-frame stream, ~4%
//	                                          larger than a single frame
//	libzstd -T0 (this package)   ratio 0.703  full parallelism, one frame
//
// The Go encoder's WithEncoderConcurrency pipelines its stages rather
// than splitting the input into jobs: it measured ~1.4x and does not
// improve past two workers, and EncodeAll ignores the setting entirely.
// libzstd splits the input into jobs, compresses them in parallel and
// stitches them into a single frame, with overlapLog preserving context
// across job boundaries — which is why zstd -T0 and -T1 produce
// byte-identical output sizes.
//
// Decompression is deliberately NOT bound here: klauspost reads what this
// writes, so restore paths need no cgo.
package zstdc

/*
#cgo CFLAGS: -O3 -DXXH_NAMESPACE=ZSTDC_
#cgo !windows CFLAGS: -pthread
#cgo !windows LDFLAGS: -pthread

#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// Compression levels, named so call sites read as intentions.
const (
	// LevelFast is zstd -3, the CLI's own default: near-free CPU.
	LevelFast = 3
	// LevelBalanced is zstd -12: meaningfully smaller, still fast enough
	// to be irrelevant beside a VACUUM.
	LevelBalanced = 12
	// LevelSmall is zstd -19, for archives kept a long time.
	LevelSmall = 19
)

// Stats describes one compression run.
type Stats struct {
	// InBytes and OutBytes are what was read and written.
	InBytes, OutBytes int64
	// WorkersUsed is what libzstd granted. Compare it against what was
	// requested: a library built without multithreading accepts the
	// request and then runs single-threaded, and looking at this number
	// is the only way to find out.
	WorkersUsed int
}

// Ratio is OutBytes/InBytes, or 0 when nothing was read.
func (s Stats) Ratio() float64 {
	if s.InBytes == 0 {
		return 0
	}
	return float64(s.OutBytes) / float64(s.InBytes)
}

// CompressFile compresses src into dst as a single zstd frame carrying a
// content checksum.
//
// workers is the number of compression threads; 0 means one per CPU. The
// returned Stats report how many libzstd actually granted.
//
// dst is not created atomically: the caller owns the temp-file-and-rename
// dance, because only the caller knows what a half-written output means
// in its context.
func CompressFile(src, dst string, level, workers int) (Stats, error) {
	if workers == 0 {
		workers = runtime.NumCPU()
	}
	cSrc := C.CString(src)
	defer C.free(unsafe.Pointer(cSrc))
	cDst := C.CString(dst)
	defer C.free(unsafe.Pointer(cDst))

	var st C.mnemo_zstd_stats
	errbuf := make([]byte, 256)
	rc := C.mnemo_zstd_compress_file(cSrc, cDst, C.int(level), C.int(workers),
		&st, (*C.char)(unsafe.Pointer(&errbuf[0])), C.size_t(len(errbuf)))
	if rc != 0 {
		return Stats{}, fmt.Errorf("zstd compress %s: %s", src, cstr(errbuf))
	}
	return Stats{
		InBytes:     int64(st.in_bytes),
		OutBytes:    int64(st.out_bytes),
		WorkersUsed: int(st.workers_used),
	}, nil
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
