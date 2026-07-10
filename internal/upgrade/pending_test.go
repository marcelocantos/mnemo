// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteLoadConsumePendingAllowlist(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	tr := NewNoticeTracker()
	if err := WritePendingNotice(home, PendingNotice{
		From:     "0.61.0",
		To:       "0.62.0",
		Sessions: []string{"s-old", "s-old", "s-other"},
	}); err != nil {
		t.Fatal(err)
	}
	path := PendingPath(home)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("pending file missing")
	}
	p, ok, err := LoadAndConsumePending(home, tr)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if p.From != "0.61.0" || p.To != "0.62.0" || len(p.Sessions) != 2 {
		t.Fatalf("pending %+v", p)
	}
	// File deleted after load.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pending file should be deleted")
	}
	// Only allowlisted sessions have notices.
	if msg, ok := tr.Consume("s-old"); !ok || msg == "" {
		t.Fatal("s-old should have notice")
	}
	if _, ok := tr.Consume("s-new"); ok {
		t.Fatal("s-new must not have notice")
	}
	// Second load is a no-op.
	_, ok, err = LoadAndConsumePending(home, tr)
	if err != nil || ok {
		t.Fatalf("second load ok=%v err=%v", ok, err)
	}
}

func TestSessionSetSnapshot(t *testing.T) {
	t.Parallel()
	s := NewSessionSet()
	s.Add("a")
	s.Add("")
	s.Add("b")
	s.Add("a")
	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot %v", got)
	}
}

func TestRouteAppendAndRead(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if RouteConfigured(home) {
		t.Fatal("expected no route")
	}
	r, err := AppendBackend(home, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if !RouteConfigured(home) || r.Primary != 0 {
		t.Fatalf("%+v", r)
	}
	r, err = AppendBackend(home, "http://127.0.0.1:2")
	if err != nil {
		t.Fatal(err)
	}
	if r.Primary != 1 || len(r.Backends) != 2 {
		t.Fatalf("%+v", r)
	}
	// file on disk
	if _, err := os.Stat(filepath.Join(home, ".mnemo", RouteFileName)); err != nil {
		t.Fatal(err)
	}
}
