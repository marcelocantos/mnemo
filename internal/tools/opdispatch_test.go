// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strings"
	"testing"
)

func testTable() opTable {
	return opTable{
		tool: "mnemo_thing",
		ops: []opSpec{
			{name: "status", desc: "report state"},
			{name: "sync", desc: "write notes", params: []string{"force"}},
			{name: "gc", desc: "clean up", params: []string{"confirm", "scope"}},
		},
	}
}

// TestResolveMissingOpNamesValidOps is convention rule 2: the failure
// teaches the answer instead of requiring a re-read of the description.
func TestResolveMissingOpNamesValidOps(t *testing.T) {
	for _, args := range []map[string]any{
		{},
		{"op": ""},
		{"op": "   "},
	} {
		_, err := testTable().resolve(args)
		if err == nil {
			t.Fatalf("args %v: want an error for a missing op", args)
		}
		for _, want := range []string{"status", "sync", "gc"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("args %v: error %q does not name op %q", args, err, want)
			}
		}
	}
}

func TestResolveUnknownOpNamesValidOps(t *testing.T) {
	_, err := testTable().resolve(map[string]any{"op": "recluster"})
	if err == nil {
		t.Fatal("want an error for an unknown op")
	}
	if !strings.Contains(err.Error(), "recluster") {
		t.Errorf("error %q does not quote the offending op", err)
	}
	for _, want := range []string{"status", "sync", "gc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name op %q", err, want)
		}
	}
}

// TestResolveRejectsForeignParam is convention rule 3, the one that
// keeps a consolidated tool from failing silently: a parameter valid
// for another op must be refused, not ignored.
func TestResolveRejectsForeignParam(t *testing.T) {
	_, err := testTable().resolve(map[string]any{"op": "sync", "confirm": true})
	if err == nil {
		t.Fatal("op=sync must reject a parameter belonging to op=gc, not ignore it")
	}
	if !strings.Contains(err.Error(), "confirm") {
		t.Errorf("error %q does not name the offending parameter", err)
	}
	// And it must say where the parameter DOES belong.
	if !strings.Contains(err.Error(), "gc") {
		t.Errorf("error %q does not point at the op that accepts it", err)
	}
}

func TestResolveRejectsUnknownParam(t *testing.T) {
	_, err := testTable().resolve(map[string]any{"op": "sync", "nonsense": 1})
	if err == nil {
		t.Fatal("want an error for a parameter no op declares")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("error %q does not name the offending parameter", err)
	}
}

func TestResolveAcceptsDeclaredParams(t *testing.T) {
	tbl := testTable()
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"op": "status"}, "status"},
		{map[string]any{"op": "sync", "force": true}, "sync"},
		{map[string]any{"op": "gc", "confirm": true, "scope": "all"}, "gc"},
	} {
		got, err := tbl.resolve(tc.args)
		if err != nil {
			t.Fatalf("args %v: unexpected error %v", tc.args, err)
		}
		if got != tc.want {
			t.Errorf("args %v: got op %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestDescribeCoversEveryOp is convention rule 4: the enumeration an
// agent reads is generated from the dispatch table, so it cannot drift
// away from what is actually accepted.
func TestDescribeCoversEveryOp(t *testing.T) {
	tbl := testTable()
	desc := tbl.describe()
	for _, o := range tbl.ops {
		if !strings.Contains(desc, "op="+o.name) {
			t.Errorf("description omits op %q", o.name)
		}
		for _, p := range o.params {
			if !strings.Contains(desc, p) {
				t.Errorf("description omits parameter %q of op %q", p, o.name)
			}
		}
	}
}
