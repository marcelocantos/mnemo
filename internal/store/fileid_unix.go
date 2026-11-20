// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package store

import (
	"os"
	"syscall"
)

// fileIdent is the identity of a transcript path. ino == 0 means unknown
// (stat failed, or this platform has no inode). It is process memory only:
// a restart learns identity from the prefix hash instead.
type fileIdent struct {
	dev uint64
	ino uint64
}

func fileIdentOf(path string) fileIdent {
	info, err := os.Stat(path)
	if err != nil {
		return fileIdent{}
	}
	return fileIdentFromInfo(info)
}

func fileIdentFromInfo(info os.FileInfo) fileIdent {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil || st.Ino == 0 {
		return fileIdent{}
	}
	return fileIdent{dev: uint64(st.Dev), ino: uint64(st.Ino)}
}
