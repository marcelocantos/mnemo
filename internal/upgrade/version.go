// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package upgrade implements connection-preserving self-upgrade pieces
// for mnemo (🎯T97): release detection, single-holder background lease,
// opt-in auto-apply orchestration, and one-time upgrade notices.
package upgrade

import (
	"fmt"
	"strconv"
	"strings"
)

// NormalizeTag strips a leading "v" and surrounding whitespace so
// "v0.61.0" and "0.61.0" compare equal as base versions.
func NormalizeTag(tag string) string {
	t := strings.TrimSpace(tag)
	t = strings.TrimPrefix(t, "v")
	t = strings.TrimPrefix(t, "V")
	return t
}

// ParseSemver parses a dotted major.minor.patch version (extra
// pre-release suffix after '-' is ignored for ordering).
func ParseSemver(tag string) (major, minor, patch int, err error) {
	t := NormalizeTag(tag)
	if t == "" {
		return 0, 0, 0, fmt.Errorf("empty version")
	}
	if i := strings.IndexAny(t, "-+"); i >= 0 {
		t = t[:i]
	}
	parts := strings.Split(t, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return 0, 0, 0, fmt.Errorf("invalid version %q", tag)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, e := strconv.Atoi(p)
		if e != nil || n < 0 {
			return 0, 0, 0, fmt.Errorf("invalid version component %q in %q", p, tag)
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], nil
}

// Compare returns -1 if a < b, 0 if a == b, 1 if a > b (semver-ish).
func Compare(a, b string) (int, error) {
	am, an, ap, err := ParseSemver(a)
	if err != nil {
		return 0, fmt.Errorf("current: %w", err)
	}
	bm, bn, bp, err := ParseSemver(b)
	if err != nil {
		return 0, fmt.Errorf("latest: %w", err)
	}
	if am != bm {
		if am < bm {
			return -1, nil
		}
		return 1, nil
	}
	if an != bn {
		if an < bn {
			return -1, nil
		}
		return 1, nil
	}
	if ap != bp {
		if ap < bp {
			return -1, nil
		}
		return 1, nil
	}
	return 0, nil
}

// IsNewer reports whether latest is strictly newer than current.
func IsNewer(current, latest string) (bool, error) {
	c, err := Compare(current, latest)
	if err != nil {
		return false, err
	}
	return c < 0, nil
}
