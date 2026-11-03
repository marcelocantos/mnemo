// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"testing"
)

// TestMain gates the entire e2e suite behind MNEMO_E2E=1.
//
// Without the env var the package reports no tests, so the standard
// `go test ./...` run (make bullseye) never executes or blocks on
// these daemon-subprocess tests. Set MNEMO_E2E=1 to opt in; `make e2e`
// does this. Main removes the built daemon binary on exit.
func TestMain(m *testing.M) {
	if os.Getenv("MNEMO_E2E") == "" {
		// Exit 0 with no output — `go test` treats this as "no tests
		// to run" and reports the package as skipped.
		os.Exit(0)
	}
	os.Exit(Main(m))
}
