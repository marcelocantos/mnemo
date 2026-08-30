// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestBackgroundWorkIsSupervised is the P2 ratchet for 🎯T154.
//
// An inventory of this package found ten background writers with no
// context and no completion signal — the image backfill, the per-session
// image extractor, the OCR / describer / embedder fan-outs among them.
// Nothing could cancel them and nothing could wait for them, so
// "is the store quiescent?" had no answer and a WAL-size assertion could
// pass on one platform and fail on another. Fixing those ten by hand
// fixes today's ten; this test is what stops an eleventh.
//
// The rule: a `go` statement in this package must either go through
// goOnce / goLoop, or be joined by its own function before that function
// returns (a local fan-out, where the caller's supervision covers the
// children). Anything else is unsupervised.
func TestBackgroundWorkIsSupervised(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "startup.go" {
			continue // the supervisor itself
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			if supervisionExemptions[fn.Name.Name] != "" {
				return true
			}
			if fn.Name.Name == "StartRateCardRefresher" {
				return true // package-level, writes no SQLite (see its comment)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				g, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				t.Errorf("%s: unsupervised `go` in %s — use s.goOnce (finite work, "+
					"covered by AwaitStartup) or s.goLoop (long-lived, cancelled and "+
					"drained by Close), or join it with wg.Wait() before returning",
					fset.Position(g.Pos()), fn.Name.Name)
				return true
			})
			return true
		})
	}
}

// supervisionExemptions names the functions whose `go` statements are
// deliberately not supervised, each with the reason. An allowlist beats a
// cleverer heuristic here: a heuristic silently reclassifies new code,
// whereas adding an entry here is a decision someone has to write down.
// That is the whole anti-recurrence property — a new background writer
// either gets supervised or gets justified in review.
var supervisionExemptions = map[string]string{
	"IngestAll": "parse fan-out joined by closing pathCh; the caller ranges " +
		"to completion, and the writer transaction commits before IngestAll returns",
	"knnSearch": "query-time similarity fan-out, joined by the caller draining " +
		"its result channel; owns no DB writes",
	"StartImageDescriber": "worker fan-out inside a goOnce that joins it with " +
		"wg.Wait() before returning, so AwaitStartup covers the whole backfill",
	"DriveStreamReconcilers": "per-stream passes with their own deadline; the " +
		"bounded wait deliberately abandons a stream that overruns rather than " +
		"holding the reconciler loop (see the abandon path). Residue: an " +
		"abandoned stream can still write after the drive returns",
}

// TestStartupCapabilitiesAreDeclared pins the capability set: a
// capability that phases provide but allCapabilities omits cannot be
// awaited or reported, which is the failure mode the graph exists to
// prevent.
func TestStartupCapabilitiesAreDeclared(t *testing.T) {
	g := newStartupGraph()
	for _, c := range []Capability{CapSchemaCurrent, CapCodecReady, CapEntriesMaterialised} {
		if _, ok := g.caps[c]; !ok {
			t.Errorf("capability %s is used but not in allCapabilities", c)
		}
	}
	if len(g.caps) != len(allCapabilities) {
		t.Errorf("graph has %d capabilities, allCapabilities lists %d", len(g.caps), len(allCapabilities))
	}
	// Every declared capability must be reported, so a stuck phase is
	// visible in doctor rather than only as a downstream hang.
	s := &Store{startup: g}
	if got := len(s.StartupReport()); got != len(allCapabilities) {
		t.Errorf("StartupReport covers %d capabilities, want %d", got, len(allCapabilities))
	}
}

// TestPhaseSkipsWhenRequirementUnavailable proves the degraded path: a
// failed phase resolves its capabilities unavailable-with-reason, its
// dependents skip rather than hang, and a consumer that declares the
// requirement is told once instead of erroring per statement. This is
// the behaviour that turns 1,337 "no column named content_z" inserts
// into one skip line.
func TestPhaseSkipsWhenRequirementUnavailable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	// Swap in a fresh graph so the assertions are about the runner, not
	// about this store's own (already resolved) boot.
	s.startup = newStartupGraph()

	dependentRan := make(chan struct{})
	s.startPhase(s.bgCtx, phase{
		name:     "provider",
		provides: []Capability{CapSchemaCurrent},
		run:      func(context.Context) error { return errors.New("migration rejected") },
	})
	s.startPhase(s.bgCtx, phase{
		name:     "dependent",
		requires: []Capability{CapSchemaCurrent},
		provides: []Capability{CapCodecReady},
		run: func(context.Context) error {
			close(dependentRan)
			return nil
		},
	})

	if s.Await(CapSchemaCurrent) {
		t.Fatal("a failed phase must resolve its capability unavailable")
	}
	if s.Await(CapCodecReady) {
		t.Fatal("a dependent of an unavailable capability must not become available")
	}
	select {
	case <-dependentRan:
		t.Fatal("dependent phase ran despite an unavailable requirement")
	default:
	}
	// AwaitStartup returns: neither phase is left pending.
	s.AwaitStartup()

	if s.Requires(CapSchemaCurrent, "docs ingest") {
		t.Error("Requires must refuse an unavailable capability")
	}
	report := s.StartupReport()
	var sawReason bool
	for _, c := range report {
		if c.Name == string(CapSchemaCurrent) {
			if c.State != "unavailable" {
				t.Errorf("schema.current state = %s, want unavailable", c.State)
			}
			sawReason = strings.Contains(c.Reason, "migration rejected")
		}
		if c.Name == string(CapEntriesMaterialised) && c.State != "pending" {
			t.Errorf("a phase that was never started should read pending, got %s", c.State)
		}
	}
	if !sawReason {
		t.Error("StartupReport must carry the failure reason")
	}
}

// TestDecodeDoesNotWaitForTheSchema is the regression test for the
// outage this graph's first deployment caused (🎯T154).
//
// loadDicts sat behind CapSchemaCurrent, so for the twelve minutes of a
// live migration every dictionary-compressed row failed to decode:
// "mnemo_text: dictionary 516157145 not loaded". Compaction's circuit
// breaker tripped and mnemo_compacted_session failed outright. Decoding
// needs only compression_dicts, which predates the migration; writing
// packed rows is what needs the new columns.
func TestDecodeDoesNotWaitForTheSchema(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.startup = newStartupGraph()

	// A migration that never finishes, standing in for the pre-migration
	// backup window.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	s.startPhase(s.bgCtx, phase{
		name:     "slow-schema",
		provides: []Capability{CapSchemaCurrent},
		run: func(ctx context.Context) error {
			select {
			case <-blocked:
			case <-ctx.Done():
			}
			return nil
		},
	})
	s.startPhase(s.bgCtx, phase{
		name:     "codec-decode",
		provides: []Capability{CapCodecDecode},
		run:      func(context.Context) error { return s.loadDicts() },
	})
	s.startPhase(s.bgCtx, phase{
		name:     "codec-write",
		requires: []Capability{CapSchemaCurrent, CapCodecDecode},
		provides: []Capability{CapCodecReady},
		run:      func(context.Context) error { return nil },
	})

	if !s.Await(CapCodecDecode) {
		t.Fatal("codec.decode must resolve while the schema phase is still running")
	}
	if s.Have(CapSchemaCurrent) {
		t.Fatal("test is not exercising the window: the schema phase already finished")
	}
	if s.Have(CapCodecReady) {
		t.Error("packed writes must still wait for the schema")
	}
}

// TestLoadDictsToleratesMissingTable: a database predating 🎯T151 has no
// compression_dicts, and no compressed rows either. Nothing to load must
// be success, or codec.decode resolves unavailable forever on the very
// boot that migrates the table in.
func TestLoadDictsToleratesMissingTable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(`DROP TABLE compression_dicts`); err != nil {
		t.Fatal(err)
	}
	if err := s.loadDicts(); err != nil {
		t.Errorf("loadDicts on a pre-🎯T151 schema: %v", err)
	}
}
