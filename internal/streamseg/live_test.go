package streamseg

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
)

// TestRealDripRoundTrip is the end-to-end witness for the claudia
// binding, and it exists because its absence shipped a broken feature.
//
// v0.72.0 went out with a Session-mode binding that could not work: Send
// drives the Claude Code TUI through tmux keystrokes, and a multi-line
// drip of a few KB is detected as a PASTE, which sits in the composer
// unsubmitted. Twelve unit tests passed the whole time, because every one
// of them used a scripted summariser. A component inventory ("Session
// mode exists and is vendored") is not a witness; only a round trip
// through the real dependency is.
//
// Env-gated because it costs model calls. Run it before believing any
// change to the binding.
func TestRealDripRoundTrip(t *testing.T) {
	if os.Getenv("MNEMO_STREAMSEG_LIVE") == "" {
		t.Skip("live: costs model calls")
	}
	wd := t.TempDir()
	s := NewClaudiaSummariser(ClaudiaSummariserOpts{WorkDir: wd, Provider: "claude", Model: "sonnet"})
	defer s.Close()

	a := New("proof", Config{SealLookahead: 2}, nil)
	msgs := []segment.Message{
		{ID: 1, Role: "user", Text: "The Windows CI job keeps timing out at 20 minutes. Any idea why?"},
		{ID: 2, Role: "assistant", Text: "Let me check the per-package timings. internal/store is taking 1055s against 23s locally."},
		{ID: 3, Role: "user", Text: "Could it be Defender scanning the temp SQLite files?"},
		{ID: 4, Role: "assistant", Text: "Plausible. I added exclusions for RUNNER_TEMP and the module cache."},
		{ID: 5, Role: "user", Text: "Did that help?"},
		{ID: 6, Role: "assistant", Text: "No. 22m42s to 21m25s, inside noise. Defender is refuted. The real cause is fsync count: 221 CREATE statements each in their own implicit transaction, times 183 test stores."},
		{ID: 7, Role: "user", Text: "So batch them into one transaction?"},
		{ID: 8, Role: "assistant", Text: "Done. internal/store went 1055s to 32.9s. Now let us talk about the release notes."},
		{ID: 9, Role: "user", Text: "What should the notes emphasise?"},
		{ID: 10, Role: "assistant", Text: "The mechanism, not the number. Readers should learn why fsync count mattered."},
	}
	fresh := a.Ingest(msgs)
	drip := renderDrip(a, fresh)
	t.Logf("drip is %d bytes, %d messages", len(drip), len(fresh))

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	start := time.Now()
	reply, err := s.Ask(ctx, drip)
	if err != nil {
		t.Fatalf("REAL DRIP FAILED: %v", err)
	}
	t.Logf("replied in %s, %d bytes", time.Since(start).Round(time.Second), len(reply))
	fmt.Println("---- RAW REPLY ----")
	fmt.Println(reply)
	fmt.Println("---- END ----")

	evs := ParseEvents(reply)
	t.Logf("parsed %d events", len(evs))
	for _, e := range evs {
		t.Logf("  %s label=%q", e, e.Label)
	}
	if len(evs) == 0 {
		t.Fatal("no parseable span events — the binding produces nothing usable")
	}
	if u, ok := s.(UsageReporter); ok {
		c, in, out, cost := u.Usage()
		t.Logf("usage: calls=%d in=%d out=%d cost=$%.4f", c, in, out, cost)
	}
}
