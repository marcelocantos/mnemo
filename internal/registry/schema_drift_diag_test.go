// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/sqlift/go/sqlift"
)

func TestSchemaCurrentRemediationDistinguishesDriftFromLock(t *testing.T) {
	drift := schemaCurrentRemediation("schema drift: not an interrupted additive migration, so a restart will not retry: Schema drift detected")
	if strings.Contains(drift, "restart to retry") {
		t.Errorf("drift remediation tells the operator to restart to retry:\n%s", drift)
	}
	if !strings.Contains(drift, "will not retry") {
		t.Errorf("drift remediation = %q, want it to say a restart will not retry", drift)
	}

	lock := schemaCurrentRemediation("schema upgrade lost the write lock after 3 attempts (rolled back; restart will retry): database is locked")
	if !strings.Contains(lock, "rolled back") || !strings.Contains(strings.ToLower(lock), "retry") {
		t.Errorf("lock remediation = %q, want a rolled-back migration that a restart retries", lock)
	}
	if strings.Contains(lock, "will not retry") {
		t.Errorf("lock remediation says the restart will not retry:\n%s", lock)
	}
}

// TestWedgedSchemaIsAFailureAndThrottledCompressIsNotOK is the health
// report from the incident: schema drift was a warning, and compression
// throttled on that drift still read as ok. A drift that is not an
// interrupted additive migration stays failed, and the packer must not
// report ok while it is throttled.
func TestWedgedSchemaIsAFailureAndThrottledCompressIsNotOK(t *testing.T) {
	// autoBackfillEnabled() re-reads LoadConfig each cycle. Without this,
	// a developer whose ~/.mnemo/config.json sets compression.auto_backfill
	// =false makes the worker report "disabled" and the wait below times out
	// with no hint that the cause is outside the repo (same trap as
	// store.isolateConfig / compress_auto_test.go, 2026-09-21).
	t.Setenv(store.MnemoHomeEnv, t.TempDir())
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mnemo.db")
	s, err := store.New(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Stamp the pre-tamper hash, then add a table the desired schema
	// does not have. The difference is a drop, which reconcile refuses.
	before, err := sqlift.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	current, err := sqlift.Extract(before)
	if err != nil {
		t.Fatal(err)
	}
	hash := current.Hash()
	before.Close()

	db, err := sql.Open(store.SQLiteDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE drift_extra (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _sqlift_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO _sqlift_state (key, value) VALUES ('schema_hash', ?)`, hash); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.New(dbPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.AwaitStartup()

	var schemaReason string
	for _, c := range s.StartupReport() {
		if c.Name == string(store.CapSchemaCurrent) {
			if c.State != "unavailable" {
				t.Fatalf("schema.current state = %s (%s), want unavailable", c.State, c.Reason)
			}
			schemaReason = c.Reason
		}
	}
	if !strings.Contains(schemaReason, "schema drift:") {
		t.Fatalf("schema.current reason = %q, want a schema drift refusal", schemaReason)
	}

	// The extra table is the operator's, not residue of a partial
	// migration. Repair must leave it in place.
	db, err = sql.Open(store.SQLiteDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'drift_extra'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if n != 1 {
		t.Fatalf("drift_extra count = %d, want 1 (reconcile must not drop it)", n)
	}

	s.StartCompressBackfill()
	deadline := time.Now().Add(2 * time.Second)
	var phase string
	for {
		phase = s.CompressWorkerStatus().Phase
		if phase == store.CompressPhaseThrottled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("compress phase = %q, want throttled", phase)
		}
		time.Sleep(10 * time.Millisecond)
	}

	r := NewRegistry(context.Background(), store.Config{}, dir)
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	rep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), true, time.Now())
	var sawSchema, sawCompress bool
	for _, res := range rep.Results {
		switch res.Name {
		case "startup.capabilities":
			sawSchema = true
			if res.Severity != "fail" {
				t.Errorf("startup.capabilities severity = %s, want fail\n%s", res.Severity, res.Detail)
			}
			if strings.Contains(res.Remediation, "restart to retry") {
				t.Errorf("remediation tells the operator to restart to retry:\n%s", res.Remediation)
			}
			if !strings.Contains(res.Remediation, "will not retry") {
				t.Errorf("remediation = %q, want it to say a restart will not retry", res.Remediation)
			}
		case "compress.backfill":
			sawCompress = true
			if res.Severity == "ok" {
				t.Errorf("compress.backfill is ok while throttled: %s", res.Detail)
			}
			if res.Severity != "warn" {
				t.Errorf("compress.backfill severity = %s, want warn\n%s", res.Severity, res.Detail)
			}
			if !strings.Contains(res.Detail, "throttled") {
				t.Errorf("compress.backfill detail = %q, want it to say throttled", res.Detail)
			}
		}
	}
	if !sawSchema {
		t.Fatal("startup.capabilities is not in the report")
	}
	if !sawCompress {
		t.Fatal("compress.backfill is not in the report")
	}
}
