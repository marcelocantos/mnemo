// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

func TestParseLsofOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]int
	}{
		{
			name:  "empty output",
			input: "",
			want:  map[string]int{},
		},
		{
			name: "header only",
			input: "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n",
			want:  map[string]int{},
		},
		{
			name: "single live session",
			input: "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"claude   1234 marcelo   3w   REG    1,5    12345 6789 /Users/marcelo/.claude/projects/-Users-foo/abc123def.jsonl\n",
			want: map[string]int{"abc123def": 1234},
		},
		{
			name: "multiple sessions different PIDs",
			input: "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"claude   1001 marcelo   3w   REG    1,5    100 100 /home/user/.claude/projects/proj1/sess-aaa.jsonl\n" +
				"claude   2002 marcelo   3w   REG    1,5    200 200 /home/user/.claude/projects/proj2/sess-bbb.jsonl\n",
			want: map[string]int{"sess-aaa": 1001, "sess-bbb": 2002},
		},
		{
			name: "non-jsonl files are ignored",
			input: "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"claude   1001 marcelo   3w   REG    1,5    100 100 /home/user/.claude/projects/proj1/some.db\n" +
				"claude   2002 marcelo   3w   REG    1,5    200 200 /home/user/.claude/projects/proj2/sess-bbb.jsonl\n",
			want: map[string]int{"sess-bbb": 2002},
		},
		{
			name: "lines with too few fields are ignored",
			input: "claude   999\n" +
				"claude   1001 marcelo   3w   REG    1,5    100 100 /home/user/.claude/projects/proj1/sess-ccc.jsonl\n",
			want: map[string]int{"sess-ccc": 1001},
		},
		{
			name: "invalid PID is ignored",
			input: "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"claude   abc  marcelo   3w   REG    1,5    100 100 /home/user/.claude/projects/proj1/sess-ddd.jsonl\n" +
				"claude   5678 marcelo   3w   REG    1,5    200 200 /home/user/.claude/projects/proj2/sess-eee.jsonl\n",
			want: map[string]int{"sess-eee": 5678},
		},
		{
			name: "same session opened by same process multiple times — deduplicates",
			input: "COMMAND   PID   USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n" +
				"claude   1234 marcelo   3w   REG    1,5    100 100 /home/user/.claude/projects/proj1/sess-fff.jsonl\n" +
				"claude   1234 marcelo   4w   REG    1,5    100 100 /home/user/.claude/projects/proj1/sess-fff.jsonl\n",
			want: map[string]int{"sess-fff": 1234},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLsofOutput([]byte(tc.input))
			if len(got) != len(tc.want) {
				t.Errorf("got %d entries, want %d: %v", len(got), len(tc.want), got)
				return
			}
			for sid, wantPID := range tc.want {
				if gotPID, ok := got[sid]; !ok {
					t.Errorf("missing session %q in result", sid)
				} else if gotPID != wantPID {
					t.Errorf("session %q: got PID %d, want %d", sid, gotPID, wantPID)
				}
			}
		})
	}
}
