// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkIngestMessages measures the writer path with the codec on and
// off (🎯T151 acceptance: under 10% regression). Rows mimic the live
// corpus's shape — a few hundred bytes of prose with tool-ish markup.
//
//	go test -tags sqlite_fts5 -run '^$' -bench BenchmarkIngestMessages -benchmem ./internal/store/
func BenchmarkIngestMessages(b *testing.B) {
	for _, mode := range []struct {
		name  string
		ready bool
	}{{"plain", false}, {"compressed", true}} {
		b.Run(mode.name, func(b *testing.B) {
			s, err := New(filepath.Join(b.TempDir(), "bench.db"), b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			s.codec.ready.Store(mode.ready)

			const perFile = 200
			msgs := make([]parsedMessage, perFile)
			for i := range msgs {
				msgs[i] = parsedMessage{
					role: "assistant", contentType: "text", timestamp: "2026-04-01T10:00:00Z", typ: "assistant",
					text: fmt.Sprintf("Reading %s:%d — the handler validates the session token, then "+
						"falls through to the retry path when the upstream returns 503. I'll add a "+
						"bounded backoff and a test that asserts the third attempt succeeds. (row %d)",
						"internal/api/handler.go", 120+i, i),
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				ws, err := s.newWriterState()
				if err != nil {
					b.Fatal(err)
				}
				sid := fmt.Sprintf("sess-%d", n)
				pf := parsedFile{
					path: filepath.Join(b.TempDir(), sid+".jsonl"), sessionID: sid, project: "bench",
					entries:  []parsedRawEntry{{entryType: "assistant", timestamp: "2026-04-01T10:00:00Z", raw: []byte(`{"type":"assistant"}`)}},
					messages: msgs,
				}
				s.writeParsedFile(ws, pf)
				ws.Close()
				if err := ws.tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
			b.SetBytes(int64(perFile * len(msgs[0].text)))
		})
	}
}
