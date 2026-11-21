// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCursorWALUpdateReachesIndexViaWatcher(t *testing.T) {
	chats := filepath.Join(t.TempDir(), "chats")
	path := filepath.Join(chats, "abc", cursorSessUUID, "store.db")
	const old = "old-wal-watch-result"
	const updated = "new-wal-watch-result"
	writeCursorStoreFixture(t, path, old, "")
	db, err := sql.Open(writerDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA wal_autocheckpoint = 0;`); err != nil {
		t.Fatal(err)
	}
	s := newTestStore(t, t.TempDir())
	s.SetCursorChatRoots([]string{chats})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	expectOnce(t, s, old)
	// Test the event dispatch independently of the safety poll.
	savedPollInterval := watchPollInterval
	watchPollInterval = time.Hour
	t.Cleanup(func() { watchPollInterval = savedPollInterval })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	watchErr := make(chan error, 1)
	go func() { watchErr <- s.Watch(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-watchErr:
			if err != nil {
				t.Errorf("watch: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("watch did not stop after cancellation")
		}
	})
	await := func(message string, ready func() bool) {
		t.Helper()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for !ready() {
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatalf("%s (watch telemetry: %+v)", message, s.WatchTelemetrySnapshot())
			}
		}
	}
	await("watch did not start", func() bool { return s.WatchTelemetrySnapshot().Running })
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE blobs SET data = CAST(REPLACE(CAST(data AS TEXT), ?, ?) AS BLOB)
		WHERE id = 'blob0'`, old, updated); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("main database changed during WAL-only update, err %v", err)
	}
	// A write to an open WAL can be silent on FSEvents. Change the
	// sidecar's metadata to deliver an explicit event for this test.
	stamp := time.Now().Add(time.Hour)
	if err := os.Chtimes(path+"-wal", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	await("WAL-only update never reached the index", func() bool { return countText(t, s, updated) == 1 })
	if n := countText(t, s, old); n != 0 {
		t.Fatalf("watch left %d old blob messages after refresh", n)
	}
}
