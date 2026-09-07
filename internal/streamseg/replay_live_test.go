// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"

	"github.com/marcelocantos/mnemo/internal/store"
)

// The streaming segmenter's spawn-shape measurement (mirrors the
// compactor's in internal/compact/replay_live_test.go).
//
// Two env-gated modes, because the sample cannot be lifted from the
// database the way the compactor's was. A compaction stores the exact
// prompt it sent; a drip does not exist anywhere durable — it is composed
// from the automaton's rolling state, which depends on every reply before
// it. So the sample is HARVESTED by replaying real production transcripts
// through the production (claudia) path once, freezing each drip prompt
// and the reply it drew, and that frozen pair is then what both arms are
// measured on.

// HarvestEnv holds the path to a read-only extract of the production
// database (messages plus compression_dicts for a handful of sessions).
// Set it to re-cut the frozen sample; it spawns claudia once per drip.
const HarvestEnv = "MNEMO_STREAMSEG_HARVEST_DB"

// ReplayEnv gates the live replay: it spawns a real claude per drip
// against the owner's account, so it runs only when asked for.
const ReplayEnv = "MNEMO_STREAMSEG_REPLAY_LIVE"

// ReplayArmEnv selects the spawn path under measurement: "bare"
// (default, claudecli) or "claudia" (the pre-change claudia.Task path).
// Both arms replay the same frozen drips, so before and after is a
// measured comparison on identical input.
const ReplayArmEnv = "MNEMO_STREAMSEG_ARM"

// replayDripTimeout bounds one drip. A drip is ~1 KB of transcript and
// lands in seconds; three minutes is generous without letting a wedged
// spawn hold the suite.
const replayDripTimeout = 3 * time.Minute

// replayConcurrency is how many drips run at once during replay. Drips
// are independent once frozen — the automaton state is baked into the
// prompt — so the wall clock is a scheduling choice, not a correctness
// one. Harvest is necessarily serial per session.
const replayConcurrency = 4

// harvestDrips is how many drips the frozen sample holds, matching the
// compactor's 20-span sample so the two measurements are comparable.
const harvestDrips = 20

// minBoundaryAgreement is the fraction of the frozen run's span
// boundaries the arm under measurement must also draw, averaged over
// drips that emitted any.
//
// Not 1.0, and not close to it. Two runs of the same model on the same
// drip disagree at the margin about whether a topic is finished — that
// is the judgement the tier exists to make, and it is stochastic. The
// floor catches a real break (a spawn that returns prose, an empty
// system prompt reinstating Claude Code's persona, a segmenter that
// stops emitting events) rather than pinning ordinary variance. The
// claudia arm is measured against the same frozen output for exactly
// this reason: its score is the noise floor the bare arm is read
// against.
const minBoundaryAgreement = 0.4

// minEmitRate is the fraction of drips that must yield at least one
// parseable event.
//
// Not 1.0: "if a drip contains nothing worth a span event, reply with no
// lines at all" is an instruction in SystemPrompt, so an empty reply is
// a legitimate production outcome, not a failure this change introduces.
const minEmitRate = 0.5

// replayDir is where the frozen sample lives.
var replayDir = filepath.Join("testdata", "replay")

// replayCase is one frozen drip: the exact user turn the production path
// composed, and the events it drew.
type replayCase struct {
	name   string
	drip   string
	before []Event
}

func loadReplayCases(t *testing.T) []replayCase {
	t.Helper()
	drips, err := filepath.Glob(filepath.Join(replayDir, "*.drip.txt"))
	if err != nil || len(drips) == 0 {
		t.Fatalf("no frozen sample under %s (glob err %v); harvest one with %s", replayDir, err, HarvestEnv)
	}
	sort.Strings(drips)
	cases := make([]replayCase, 0, len(drips))
	for _, p := range drips {
		name := strings.TrimSuffix(filepath.Base(p), ".drip.txt")
		drip, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		beforeRaw, err := os.ReadFile(filepath.Join(replayDir, name+".before.jsonl"))
		if err != nil {
			t.Fatalf("read before events for %s: %v", name, err)
		}
		cases = append(cases, replayCase{name: name, drip: string(drip), before: ParseEvents(string(beforeRaw))})
	}
	return cases
}

// cuts is the boundary set an event stream draws: the message id of every
// span edge it names.
//
// Boundary placement is what the tier is for, so it is what the two arms
// are compared on. Labels and summaries are prose and differ between any
// two runs; a cut is an integer the model either agrees on or does not.
func cuts(events []Event) map[int]bool {
	out := map[int]bool{}
	for _, ev := range events {
		if ev.From > 0 {
			out[int(ev.From)] = true
		}
		if ev.To > 0 {
			out[int(ev.To)] = true
		}
	}
	return out
}

// agreement is the fraction of want's cuts that got also drew. Same
// shape as the compactor replay's file recall: it asks whether the
// cheaper spawn still sees what the expensive one saw.
func agreement(got, want map[int]bool) (float64, bool) {
	if len(want) == 0 {
		return 0, false
	}
	var hits int
	for c := range want {
		if got[c] {
			hits++
		}
	}
	return float64(hits) / float64(len(want)), true
}

// TestReplayBareSpawnHoldsBoundaries measures both spawn paths over the
// frozen drips and reports what each cost and how much of the frozen
// run's boundary placement it reproduced.
//
// This is the acceptance oracle for replacing claudia.Task with
// claudecli in the segmenter. The cost half of the claim is measured on
// real drips rather than extrapolated from a probe, and the boundary half
// is what stops the cost half from being won by segmenting badly.
func TestReplayBareSpawnHoldsBoundaries(t *testing.T) {
	if os.Getenv(ReplayEnv) == "" {
		t.Skipf("live replay: set %s=1 (spawns claude once per drip, spends real money)", ReplayEnv)
	}
	cases := loadReplayCases(t)
	workDir := t.TempDir()

	arm := os.Getenv(ReplayArmEnv)
	opts := ClaudiaSummariserOpts{WorkDir: workDir, Provider: "claude", Model: "sonnet"}
	var summ Summariser
	switch arm {
	case "", "bare":
		arm = "bare"
		summ = NewSummariser(opts)
	case "claudia":
		summ = NewClaudiaSummariser(opts)
	default:
		t.Fatalf("%s=%q: want bare or claudia", ReplayArmEnv, arm)
	}
	defer summ.Close()
	t.Logf("arm=%s drips=%d", arm, len(cases))

	type outcome struct {
		name      string
		events    int
		agreed    float64
		scored    bool
		emitted   bool
		callError error
	}
	outcomes := make([]outcome, len(cases))

	var wg sync.WaitGroup
	sem := make(chan struct{}, replayConcurrency)
	for i, tc := range cases {
		wg.Add(1)
		go func(i int, tc replayCase) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), replayDripTimeout)
			reply, err := summ.Ask(ctx, tc.drip)
			cancel()
			if err != nil {
				outcomes[i] = outcome{name: tc.name, callError: err}
				return
			}
			got := ParseEvents(reply)
			a, scored := agreement(cuts(got), cuts(tc.before))
			outcomes[i] = outcome{name: tc.name, events: len(got), agreed: a, scored: scored, emitted: len(got) > 0}
		}(i, tc)
	}
	wg.Wait()

	var (
		emitted, scoredN int
		totalAgreed      float64
	)
	for _, o := range outcomes {
		if o.callError != nil {
			t.Errorf("%s: ask: %v", o.name, o.callError)
			continue
		}
		if o.emitted {
			emitted++
		}
		if o.scored {
			scoredN++
			totalAgreed += o.agreed
		}
		t.Logf("%s: events=%d boundary_agreement=%.2f scored=%v", o.name, o.events, o.agreed, o.scored)
	}

	calls, inTok, outTok, cost := 0, 0, 0, 0.0
	if u, ok := summ.(UsageReporter); ok {
		calls, inTok, outTok, cost = u.Usage()
	}
	var avgAgreed float64
	if scoredN > 0 {
		avgAgreed = totalAgreed / float64(scoredN)
	}
	avgPrompt := 0
	if calls > 0 {
		avgPrompt = inTok / calls
	}
	t.Logf("SUMMARY arm=%s drips=%d calls=%d prompt_tokens=%d output_tokens=%d cost=$%.4f avg_prompt=%d emitted=%d avg_boundary_agreement=%.2f (scored on %d)",
		arm, len(cases), calls, inTok, outTok, cost, avgPrompt, emitted, avgAgreed, scoredN)

	if rate := float64(emitted) / float64(len(cases)); rate < minEmitRate {
		t.Errorf("only %d/%d drips emitted an event (%.2f < %.2f)", emitted, len(cases), rate, minEmitRate)
	}
	if scoredN == 0 {
		t.Fatal("no drip could be scored: the frozen sample has no boundaries")
	}
	if avgAgreed < minBoundaryAgreement {
		t.Errorf("avg boundary agreement %.2f < %.2f: this spawn is placing boundaries the frozen run did not",
			avgAgreed, minBoundaryAgreement)
	}
}

// TestHarvestReplaySample cuts the frozen sample, replaying real
// production transcripts through the claudia path exactly as the watcher
// would and freezing each drip with the events it drew.
//
// Not a test in the ordinary sense — it writes testdata and spends money.
// It lives here rather than in a cmd/ tool because it needs the
// unexported drip renderer: freezing a prompt some other code composed
// would measure that other code.
func TestHarvestReplaySample(t *testing.T) {
	dbPath := os.Getenv(HarvestEnv)
	if dbPath == "" {
		t.Skipf("harvest: set %s to a read-only production extract", HarvestEnv)
	}
	sessions, err := loadSampleSessions(dbPath)
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	if err := os.MkdirAll(replayDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", replayDir, err)
	}
	workDir := t.TempDir()

	var frozen int
	for _, g := range sessions {
		if frozen >= harvestDrips {
			break
		}
		inner := NewClaudiaSummariser(ClaudiaSummariserOpts{WorkDir: workDir, Provider: "claude", Model: "sonnet"})
		cap := &capturingSummariser{inner: inner, dir: replayDir, next: &frozen, limit: harvestDrips}
		rs := &replayStore{msgs: g.Messages}
		r := &Runner{SessionID: g.SessionID, Store: rs, Summ: cap, DripSize: DefaultDripSize}
		if err := r.Start(); err != nil {
			t.Fatalf("%s: start: %v", g.SessionID, err)
		}
		for frozen < harvestDrips {
			ctx, cancel := context.WithTimeout(context.Background(), replayDripTimeout)
			n, err := r.Step(ctx)
			cancel()
			if err != nil {
				t.Logf("%s: step: %v", g.SessionID, err)
				break
			}
			if n == 0 {
				break
			}
		}
		inner.Close()
		t.Logf("%s: %d drips frozen so far", g.SessionID, frozen)
	}
	if frozen == 0 {
		t.Fatal("harvested nothing")
	}
	t.Logf("HARVEST drips=%d dir=%s", frozen, replayDir)
}

// capturingSummariser writes each drip and its reply to the frozen
// sample as the runner drives it.
type capturingSummariser struct {
	inner Summariser
	dir   string
	next  *int
	limit int
}

func (c *capturingSummariser) Ask(ctx context.Context, drip string) (string, error) {
	reply, err := c.inner.Ask(ctx, drip)
	if err != nil {
		return "", err
	}
	if *c.next >= c.limit {
		return reply, nil
	}
	*c.next++
	name := fmt.Sprintf("%02d", *c.next)
	if err := os.WriteFile(filepath.Join(c.dir, name+".drip.txt"), []byte(drip), 0o644); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ev := range ParseEvents(reply) {
		b.WriteString(eventJSON(ev))
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(c.dir, name+".before.jsonl"), []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return reply, nil
}

func (c *capturingSummariser) Restart(ctx context.Context) error { return c.inner.Restart(ctx) }
func (c *capturingSummariser) Close()                            {}

// eventJSON re-renders a parsed event as one JSONL line. Round-tripping
// through the parser rather than freezing the raw reply is deliberate:
// the frozen file then holds exactly what the automaton would have acted
// on, with the model's prose and fencing already discarded.
func eventJSON(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"event":%q,"span":%q`, string(ev.Kind), ev.Ref)
	if ev.From > 0 {
		fmt.Fprintf(&b, `,"from":%d`, ev.From)
	}
	if ev.To > 0 {
		fmt.Fprintf(&b, `,"to":%d`, ev.To)
	}
	if ev.Label != "" {
		fmt.Fprintf(&b, `,"label":%q`, ev.Label)
	}
	if ev.Summary != "" {
		fmt.Fprintf(&b, `,"summary":%q`, ev.Summary)
	}
	if ev.By != "" {
		fmt.Fprintf(&b, `,"by":%q`, ev.By)
	}
	if ev.Reason != "" {
		fmt.Fprintf(&b, `,"reason":%q`, ev.Reason)
	}
	b.WriteString("}")
	return b.String()
}

// loadSampleSessions reads the extract: every session's substantive
// messages, in id order, longest session first.
//
// The extract is its own two tables (sample_messages, sample_dicts), not
// a mnemo schema, and its text column is deliberately not called
// messages.text: this harness decodes the zstd frame itself rather than
// through mnemo_text, because registering that SQL function means
// opening a store, and a harness that can open a mnemo database
// read-write is a harness that can write to the production one. The
// 🎯T151 ratchet governs readers of mnemo's own tables; the decode here
// is the same operation, spelled out.
func loadSampleSessions(path string) ([]GoldSession, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	dictRows, err := db.Query(`SELECT dict FROM sample_dicts`)
	if err != nil {
		return nil, fmt.Errorf("dicts: %w", err)
	}
	var dicts [][]byte
	for dictRows.Next() {
		var d []byte
		if err := dictRows.Scan(&d); err != nil {
			dictRows.Close()
			return nil, err
		}
		dicts = append(dicts, d)
	}
	dictRows.Close()
	opts := []zstd.DOption{}
	for _, d := range dicts {
		opts = append(opts, zstd.WithDecoderDicts(d))
	}
	dec, err := zstd.NewReader(nil, opts...)
	if err != nil {
		return nil, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	rows, err := db.Query(`
		SELECT id, session_id, role, COALESCE(body, ''), COALESCE(timestamp, ''), body_z
		FROM sample_messages WHERE is_noise = 0 ORDER BY session_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySession := map[string][]store.StreamMessage{}
	var order []string
	for rows.Next() {
		var (
			m   store.StreamMessage
			sid string
			z   []byte
		)
		if err := rows.Scan(&m.ID, &sid, &m.Role, &m.Text, &m.Timestamp, &z); err != nil {
			return nil, err
		}
		if len(z) > 0 {
			out, err := dec.DecodeAll(z, nil)
			if err != nil {
				return nil, fmt.Errorf("decode message %d: %w", m.ID, err)
			}
			m.Text = string(out)
		}
		if _, seen := bySession[sid]; !seen {
			order = append(order, sid)
		}
		bySession[sid] = append(bySession[sid], m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]GoldSession, 0, len(order))
	for _, sid := range order {
		out = append(out, GoldSession{SessionID: sid, Messages: bySession[sid]})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].Messages) > len(out[j].Messages) })
	return out, nil
}
