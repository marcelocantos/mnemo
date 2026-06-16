// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Generator writes a synthetic ~/.claude/projects/-style transcript
// tree under a project directory. Output is fully deterministic in
// the (Seed, Sessions, Repos, MsgsDist, TokensDist, ToolUseRate)
// tuple: identical configs produce byte-identical files. Intended
// for tests that need realistic-shaped fixtures without taking a
// dependency on real session data.
//
// The output mimics what Claude Code actually writes to
// ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl. Each
// session file holds a leading user prompt followed by a chain of
// assistant messages, optionally interleaved with tool_use blocks.
// All envelope fields the ingest path inspects (type, timestamp,
// sessionId, parentUuid, cwd, gitBranch, message.{role,model,usage})
// are populated.
//
// Sizing: the generator is engineered for both small fixtures (tens
// of sessions, sub-second) and mid-scale (10k+ sessions, single
// digit minutes). No premature optimisation; the hot loop is a
// straightforward JSONL writer.
type Generator struct {
	Seed        int64        // deterministic; same seed → same output
	Sessions    int          // total sessions to write
	Repos       []string     // repo names ("org/name") to distribute sessions across; round-robin
	MsgsDist    Distribution // messages per session
	TokensDist  Distribution // output_tokens per assistant message
	ToolUseRate float64      // 0..1; fraction of assistant messages that include a tool_use block

	// Model is the model slug to record on assistant messages. Empty
	// defaults to "claude-sonnet-4-6" to match the production shape
	// the cost-estimation code recognises.
	Model string

	// Epoch is the start of generated timestamps. Zero defaults to
	// 2026-01-01T00:00:00Z. Per-session offsets are deterministic.
	Epoch time.Time
}

// Distribution is a clamped triangular-style sampling spec. The
// generator draws integers in [Min, Max] with a mean targeted at
// Mean. Values outside [Min, Max] are clamped after sampling, so
// extreme Mean settings near a bound will skew toward that bound.
type Distribution struct {
	Min, Max int // hard bounds (inclusive)
	Mean     int // target mean; sampling centres around this
}

// sample draws an int in [d.Min, d.Max] biased around d.Mean. Uses
// a symmetric triangular sample: average of two uniforms scaled to
// the bracket of the chosen side. Cheap, deterministic, no dep on
// math/big or distuv.
func (d Distribution) sample(r *rand.Rand) int {
	if d.Max < d.Min {
		return d.Min
	}
	if d.Max == d.Min {
		return d.Min
	}
	mean := d.Mean
	if mean < d.Min {
		mean = d.Min
	}
	if mean > d.Max {
		mean = d.Max
	}
	// Choose side relative to mean with probability proportional to
	// each side's width — this keeps the expected value at mean
	// regardless of asymmetry between (mean-Min) and (Max-mean).
	lo := mean - d.Min
	hi := d.Max - mean
	span := lo + hi
	if span == 0 {
		return mean
	}
	var v int
	if r.Intn(span) < lo {
		// Lower side: triangular peaked at mean, falling to Min.
		// Take the max of two uniforms in [0, lo] so the density
		// peaks near lo (i.e. near mean).
		a, b := r.Intn(lo+1), r.Intn(lo+1)
		if a < b {
			a = b
		}
		v = d.Min + a
	} else {
		a, b := r.Intn(hi+1), r.Intn(hi+1)
		if a < b {
			a = b
		}
		v = mean + (hi - a)
	}
	if v < d.Min {
		v = d.Min
	}
	if v > d.Max {
		v = d.Max
	}
	return v
}

// Write materialises the configured transcript tree under
// projectsDir. The directory is created if missing. Existing files
// with colliding names are overwritten (the generator is intended
// for tempdir use).
func (g *Generator) Write(projectsDir string) error {
	if g.Sessions <= 0 {
		return errors.New("storetest.Generator: Sessions must be > 0")
	}
	if len(g.Repos) == 0 {
		return errors.New("storetest.Generator: Repos must be non-empty")
	}
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		return err
	}

	model := g.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	epoch := g.Epoch
	if epoch.IsZero() {
		epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	// Pre-compute per-repo cwd and encoded project-dir name. The
	// encoding mirrors Claude Code's convention: replace `/` and
	// `.` with `-`, prefix with `-`. The cwd must contain
	// `/work/github.com/<repo>` for mnemo's extractRepo regex to
	// populate session_meta.repo correctly.
	type repoInfo struct {
		repo      string
		cwd       string
		projDir   string // basename under projectsDir
		gitBranch string
	}
	repos := make([]repoInfo, len(g.Repos))
	for i, name := range g.Repos {
		cwd := "/Users/synth/work/github.com/" + name
		repos[i] = repoInfo{
			repo:      name,
			cwd:       cwd,
			projDir:   encodeProjectDir(cwd),
			gitBranch: "synthetic/main",
		}
	}

	// One RNG per (this Write call). Sessions are emitted in index
	// order; the same Seed therefore yields identical output.
	r := rand.New(rand.NewSource(g.Seed))

	for s := 0; s < g.Sessions; s++ {
		info := repos[s%len(repos)]
		sessionUUID := deterministicUUID(g.Seed, "session", s)
		// Stable per-session start: 1 minute apart, deterministic.
		sessionStart := epoch.Add(time.Duration(s) * time.Minute)

		projDirPath := filepath.Join(projectsDir, info.projDir)
		if err := os.MkdirAll(projDirPath, 0o755); err != nil {
			return err
		}
		filePath := filepath.Join(projDirPath, sessionUUID+".jsonl")
		f, err := os.Create(filePath)
		if err != nil {
			return err
		}

		// JSON encoder bound to a per-file writer. encoder.Encode
		// emits one JSON value per line — exactly the JSONL shape
		// Claude Code uses.
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false)

		// 1. Initial user prompt. parentUuid=nil distinguishes the
		// session root entry, matching the real on-disk shape.
		userUUID := deterministicUUID(g.Seed, "user", s, 0)
		userTS := sessionStart.Format(time.RFC3339)
		userText := fmt.Sprintf("task %d: explore the %s subsystem and propose a fix", s, info.repo)
		userEntry := map[string]any{
			"parentUuid": nil,
			"type":       "user",
			"uuid":       userUUID,
			"sessionId":  sessionUUID,
			"timestamp":  userTS,
			"cwd":        info.cwd,
			"gitBranch":  info.gitBranch,
			"userType":   "external",
			"version":    "2.1.172",
			"message": map[string]any{
				"role":    "user",
				"content": userText,
			},
		}
		if err := enc.Encode(userEntry); err != nil {
			f.Close()
			return err
		}

		// 2. N assistant messages chained from the user entry.
		nMsgs := g.MsgsDist.sample(r)
		prevUUID := userUUID
		// Each message advances time by a deterministic small step
		// derived from the rng so order is preserved without
		// per-message bookkeeping.
		curTime := sessionStart.Add(10 * time.Second)
		for m := 0; m < nMsgs; m++ {
			outTokens := g.TokensDist.sample(r)
			// Filler text rougly proportional to tokens — mnemo's
			// own ingest treats token *fields* as authoritative
			// for usage; the text just needs to be substantive
			// enough to pass the noise / boilerplate filters.
			fillerLen := outTokens * 4 // ~4 chars per token
			if fillerLen < 32 {
				fillerLen = 32
			}
			fillerSeed := uint64(g.Seed) ^ uint64(s*1_000_003+m*97)
			text := fillerText(fillerSeed, fillerLen)

			assistantUUID := deterministicUUID(g.Seed, "asst", s, m)
			content := []any{
				map[string]any{"type": "text", "text": text},
			}
			// Tool use: probabilistic per-message. The tool name and
			// input shape just need to be parseable — mnemo doesn't
			// validate against real tool schemas.
			if r.Float64() < g.ToolUseRate {
				toolUUID := deterministicUUID(g.Seed, "tool", s, m)
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    toolUUID,
					"name":  "Read",
					"input": map[string]any{"file_path": fmt.Sprintf("/synth/repo/%s/file_%d.go", info.repo, m)},
				})
			}

			inputTokens := outTokens / 10
			if inputTokens < 1 {
				inputTokens = 1
			}
			cacheCreate := outTokens * 2
			assistantEntry := map[string]any{
				"parentUuid": prevUUID,
				"type":       "assistant",
				"uuid":       assistantUUID,
				"sessionId":  sessionUUID,
				"timestamp":  curTime.Format(time.RFC3339),
				"cwd":        info.cwd,
				"gitBranch":  info.gitBranch,
				"userType":   "external",
				"version":    "2.1.172",
				"message": map[string]any{
					"role":    "assistant",
					"model":   model,
					"content": content,
					"usage": map[string]any{
						"input_tokens":                inputTokens,
						"output_tokens":               outTokens,
						"cache_creation_input_tokens": cacheCreate,
						"cache_read_input_tokens":     0,
					},
				},
			}
			if err := enc.Encode(assistantEntry); err != nil {
				f.Close()
				return err
			}
			prevUUID = assistantUUID
			// 5-second deterministic step. Real sessions vary but
			// the indexer doesn't depend on the spacing.
			curTime = curTime.Add(5 * time.Second)
		}

		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// encodeProjectDir mirrors Claude Code's convention: cwd path with
// `/` and `.` collapsed to `-`. Leading slash also becomes `-` so
// `/Users/foo` → `-Users-foo`.
func encodeProjectDir(cwd string) string {
	enc := strings.ReplaceAll(cwd, "/", "-")
	enc = strings.ReplaceAll(enc, ".", "-")
	return enc
}

// deterministicUUID derives a stable RFC 4122-shaped string from
// the seed and a tag set. The bytes are SHA-256(seed||tag||args)
// rather than v4 random, but conform to the canonical hyphenated
// 36-char layout that mnemo's session-id handling expects. Variant
// and version bits are stamped so the output is a syntactically
// valid v4 UUID, indistinguishable from a real one to consumers
// that don't crypto-verify provenance.
func deterministicUUID(seed int64, tag string, ints ...int) string {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(seed))
	h.Write(buf[:])
	h.Write([]byte(tag))
	for _, n := range ints {
		binary.BigEndian.PutUint64(buf[:], uint64(n))
		h.Write(buf[:])
	}
	sum := h.Sum(nil)[:16]
	// RFC 4122: version 4, variant 10xx (RFC 4122 variant).
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// fillerText returns a deterministic lorem-ipsum-style block of
// roughly nChars characters. Output is whitespace-separated words
// drawn from a fixed corpus, seeded by the supplied uint64. This
// keeps the per-message body realistic-looking (passes mnemo's
// isNoise/isBoilerplate filters) while remaining cheap to generate.
func fillerText(seed uint64, nChars int) string {
	words := []string{
		"lorem", "ipsum", "dolor", "sit", "amet", "consectetur",
		"adipiscing", "elit", "sed", "do", "eiusmod", "tempor",
		"incididunt", "ut", "labore", "et", "dolore", "magna",
		"aliqua", "ad", "minim", "veniam", "quis", "nostrud",
		"exercitation", "ullamco", "laboris", "nisi", "aliquip",
		"commodo", "consequat", "duis", "aute", "irure",
		"reprehenderit", "voluptate", "velit", "esse", "cillum",
		"refactor", "implement", "deploy", "subsystem", "module",
		"interface", "registry", "scheduler", "compactor", "vault",
	}
	r := rand.New(rand.NewSource(int64(seed)))
	var b strings.Builder
	b.Grow(nChars + 32)
	for b.Len() < nChars {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[r.Intn(len(words))])
	}
	return b.String()
}
