// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package iterm

import (
	"context"
	"strings"
	"testing"
)

// TestGoUnsupportedOffDarwin asserts that on a non-macOS host the `go` verb
// fails with a clear "macOS-only" message rather than a cryptic exec error
// ("osascript: executable file not found"). This file is excluded on darwin,
// where the live osascript path is instead covered by the runner-overriding
// tests in iterm_test.go.
func TestGoUnsupportedOffDarwin(t *testing.T) {
	_, err := Go(context.Background(), GoArgs{Path: "/p", Name: "demo"})
	if err == nil || !strings.Contains(err.Error(), "only available on macOS") {
		t.Fatalf("want a macOS-only error off darwin, got: %v", err)
	}
}
