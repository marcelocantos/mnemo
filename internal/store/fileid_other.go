// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package store

// fileIdent is the identity of a transcript path. On non-unix platforms
// it stays zero and resume falls back to the prefix hash.
type fileIdent struct {
	dev uint64
	ino uint64
}

func fileIdentOf(string) fileIdent { return fileIdent{} }
