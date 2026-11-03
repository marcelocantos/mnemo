// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"github.com/marcelocantos/mnemo/internal/e2e"
)

// TestMain routes this package's tests through e2e.Main so the daemon
// binary that federation_optin_test.go builds via e2e.Start is removed
// from $TMPDIR when the tests finish.
func TestMain(m *testing.M) { os.Exit(e2e.Main(m)) }
