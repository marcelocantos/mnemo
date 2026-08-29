// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

const maxWriteBytes = 32 << 20 // 32 MiB per E10

// ResolveKey maps an absolute filesystem path to a quarantine-relative key.
func ResolveKey(absPath, cwd, repo string) (key string, ok bool) {
	absPath = filepath.Clean(absPath)
	if !filepath.IsAbs(absPath) && cwd != "" {
		absPath = filepath.Clean(filepath.Join(cwd, absPath))
	}
	if absPath == "" || absPath == string(filepath.Separator) {
		return "", false
	}

	if root := findGitRoot(absPath); root != "" && repo != "" {
		if rel, err := filepath.Rel(root, absPath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return repoSlug(repo) + "/" + filepath.ToSlash(rel), true
		}
	}
	if cwd != "" {
		cwd = filepath.Clean(cwd)
		if rel, err := filepath.Rel(cwd, absPath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "by-cwd/" + cwdHash(cwd) + "/" + filepath.ToSlash(rel), true
		}
	}
	return "", false
}

// QuarantinePath joins root and key, refusing escapes.
func QuarantinePath(root, key string) (full string, err error) {
	root = filepath.Clean(root)
	key = filepath.FromSlash(key)
	full = filepath.Clean(filepath.Join(root, key))
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errPathEscape
	}
	return full, nil
}

var errPathEscape = os.ErrInvalid

func repoSlug(repo string) string {
	return strings.ReplaceAll(repo, "/", "--")
}

func cwdHash(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:8]
}

func findGitRoot(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if dir == "" || dir == string(filepath.Separator) {
			return ""
		}
		if isGitDir(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func isGitDir(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// IsInsideGitWorkTree reports whether path is inside a git working tree.
func IsInsideGitWorkTree(path string) (inside bool, root string) {
	path = filepath.Clean(path)
	fi, err := os.Lstat(path)
	if err != nil {
		return false, ""
	}
	check := path
	if fi.Mode().IsRegular() || fi.Mode()&os.ModeDir == 0 {
		check = filepath.Dir(path)
	}
	root = findGitRoot(check)
	if root == "" {
		return false, ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, root
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), root
}

func pathHasSymlinkComponent(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return true
	}
	cur := root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}
