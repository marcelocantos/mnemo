// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Forensics verification (🎯T150.9 / T150.10): manufacturable disasters +
// metamorphic relations. We do not enumerate lost-laptop stories — we plant
// known T0 trees, destroy sources on purpose, and assert invariants.

func TestDisasterPerfectBaseTwin(t *testing.T) {
	fix := plantDisaster(t)
	ops := patchOnlyTape(fix)

	twinRoot := t.TempDir()
	twinSeeds := seedsFromDir(fix.cwd, ops)
	twinRep, err := NewEngine(DefaultConfig()).Run(twinRoot, ops, twinSeeds...)
	if err != nil {
		t.Fatal(err)
	}

	// Destroy live tree; recover via git commit-at-t0 only.
	for rel := range fix.t0 {
		_ = os.Remove(filepath.Join(fix.cwd, filepath.FromSlash(rel)))
	}
	off := false
	on := true
	cfg := SeedConfig{
		UseWorkTree:    &off,
		UseGitCommit:   &on,
		UseGitStash:    &off,
		UseRewind:      &off,
		UseFileHist:    &off,
		UseReadResults: &off,
	}
	seeds, _ := ResolveSeeds(nil, ops, cfg)
	if len(seeds) == 0 {
		t.Fatal("expected git_commit seeds after worktree wipe")
	}
	for _, s := range seeds {
		if s.Source != SeedGitCommit {
			t.Fatalf("source=%s want git_commit", s.Source)
		}
	}

	recRoot := t.TempDir()
	recRep, err := NewEngine(DefaultConfig()).Run(recRoot, ops, seeds...)
	if err != nil {
		t.Fatal(err)
	}

	assertReportsMatchOnRecovered(t, twinRep, twinRoot, recRep, recRoot)
	if twinRep.OpsSkipped != 0 || recRep.OpsSkipped != 0 {
		t.Fatalf("twin skipped=%d recovered skipped=%d (want 0)", twinRep.OpsSkipped, recRep.OpsSkipped)
	}
}

func TestDisasterNoSilentInvention(t *testing.T) {
	ds := plantDisaster(t)
	ops := patchOnlyTape(ds)
	for rel := range ds.t0 {
		_ = os.Remove(filepath.Join(ds.cwd, filepath.FromSlash(rel)))
	}
	// Empty orphan repo with no matching blobs — disable every external seed.
	cfg := SeedConfig{DisableAll: true}
	seeds, _ := ResolveSeeds(nil, ops, cfg)
	if len(seeds) != 0 {
		t.Fatalf("DisableAll must yield no seeds, got %d", len(seeds))
	}
	root := t.TempDir()
	rep, err := NewEngine(DefaultConfig()).Run(root, ops, seeds...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OpsApplied != 0 {
		t.Fatalf("applied=%d want 0 (no invented empty bases)", rep.OpsApplied)
	}
	for _, r := range rep.Results {
		if r.Outcome != OutcomeSkipped || r.Reason != ReasonPatchNoBase {
			t.Fatalf("got %s/%s want skipped/%s", r.Outcome, r.Reason, ReasonPatchNoBase)
		}
	}
}

func TestDisasterSourceAblationShrinks(t *testing.T) {
	cwd := t.TempDir()
	sid := "sess-ablation"
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	repo := "acme/ablation"

	// Three paths, each only recoverable from one rung.
	paths := map[string]struct {
		rel  string
		body string
		via  SeedSource
	}{
		"wt":  {rel: "only_wt.txt", body: "from-worktree\n", via: SeedWorkTree},
		"git": {rel: "only_git.txt", body: "from-git\n", via: SeedGitCommit},
		"rw":  {rel: "ui/only_rewind.css", body: ".rw{}\n", via: SeedGrokRewind},
	}

	gitInit(t, cwd)
	// Commit only the git-backed file, then remove it so worktree cannot seed it.
	writeRel(t, cwd, paths["git"].rel, paths["git"].body)
	gitCommit(t, cwd, "seed git path", t0.Add(-24*time.Hour))
	_ = os.Remove(filepath.Join(cwd, filepath.FromSlash(paths["git"].rel)))

	writeRel(t, cwd, paths["wt"].rel, paths["wt"].body)
	_ = os.Chtimes(filepath.Join(cwd, paths["wt"].rel), t0, t0)

	grokHome := writeGrokRewind(t, sid, cwd, map[string]string{
		paths["rw"].rel: paths["rw"].body,
	}, t0.Add(-time.Hour))

	var ops []Op
	for _, p := range paths {
		abs := filepath.Join(cwd, filepath.FromSlash(p.rel))
		ops = append(ops, Op{
			Timestamp: t0.Add(time.Minute),
			SessionID: sid,
			Source:    "grok",
			Path:      abs,
			CWD:       cwd,
			Repo:      repo,
			Kind:      KindPatch,
			OldString: strings.TrimSpace(p.body),
			NewString: strings.TrimSpace(p.body) + "-patched",
		})
	}

	full := resolveWith(t, ops, SeedConfig{
		GrokHome: grokHome,
	})
	gotFull := sourcesByPath(full)
	for _, p := range paths {
		abs := filepath.Join(cwd, filepath.FromSlash(p.rel))
		if gotFull[abs] != p.via {
			// worktree wins over git when both exist; git path has no worktree file.
			t.Fatalf("%s: source=%s want %s (map=%v)", p.rel, gotFull[abs], p.via, gotFull)
		}
	}

	off, on := false, true
	ablations := []struct {
		name string
		cfg  SeedConfig
		want int
		deny SeedSource
	}{
		{"no_worktree", SeedConfig{GrokHome: grokHome, UseWorkTree: &off, UseGitCommit: &on, UseRewind: &on, UseGitStash: &off, UseFileHist: &off, UseReadResults: &off}, 2, SeedWorkTree},
		{"no_git", SeedConfig{GrokHome: grokHome, UseWorkTree: &on, UseGitCommit: &off, UseRewind: &on, UseGitStash: &off, UseFileHist: &off, UseReadResults: &off}, 2, SeedGitCommit},
		{"no_rewind", SeedConfig{GrokHome: grokHome, UseWorkTree: &on, UseGitCommit: &on, UseRewind: &off, UseGitStash: &off, UseFileHist: &off, UseReadResults: &off}, 2, SeedGrokRewind},
	}
	for _, ab := range ablations {
		t.Run(ab.name, func(t *testing.T) {
			seeds := resolveWith(t, ops, ab.cfg)
			if len(seeds) != ab.want {
				t.Fatalf("seeds=%d want %d (%v)", len(seeds), ab.want, sourcesByPath(seeds))
			}
			for _, s := range seeds {
				if s.Source == ab.deny {
					t.Fatalf("ablation still returned %s", ab.deny)
				}
			}
			// Ablation never invents content for a path the full ladder lacked.
			fullPaths := map[string]struct{}{}
			for _, s := range full {
				fullPaths[s.AbsPath] = struct{}{}
			}
			for _, s := range seeds {
				if _, ok := fullPaths[s.AbsPath]; !ok {
					t.Fatalf("ablation grew path %s not in full ladder", s.AbsPath)
				}
			}
		})
	}
}

func TestDisasterProvenanceOrder(t *testing.T) {
	cwd := t.TempDir()
	t0 := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	rel := "conflict.txt"
	abs := filepath.Join(cwd, rel)
	gitInit(t, cwd)
	writeRel(t, cwd, rel, "from-git\n")
	gitCommit(t, cwd, "git body", t0.Add(-time.Hour))

	writeRel(t, cwd, rel, "from-worktree\n")
	_ = os.Chtimes(abs, t0, t0)

	seedDir := t.TempDir()
	writeRel(t, seedDir, rel, "from-cli\n")

	ops := []Op{{
		Timestamp: t0.Add(time.Minute),
		SessionID: "prov",
		Path:      abs,
		CWD:       cwd,
		Repo:      "acme/prov",
		Kind:      KindPatch,
		OldString: "x",
		NewString: "y",
	}}

	cli := resolveWith(t, ops, SeedConfig{SeedFrom: seedDir})
	if len(cli) != 1 || cli[0].Source != SeedCLI || string(cli[0].Body) != "from-cli\n" {
		t.Fatalf("CLI must win: %+v %q", cli, bodyOf(cli))
	}

	off := false
	on := true
	wt := resolveWith(t, ops, SeedConfig{
		UseWorkTree: &on, UseGitCommit: &on, UseGitStash: &off, UseRewind: &off, UseFileHist: &off, UseReadResults: &off,
	})
	if len(wt) != 1 || wt[0].Source != SeedWorkTree || string(wt[0].Body) != "from-worktree\n" {
		t.Fatalf("worktree must beat git: %+v %q", wt, bodyOf(wt))
	}

	gitOnly := resolveWith(t, ops, SeedConfig{
		UseWorkTree: &off, UseGitCommit: &on, UseGitStash: &off, UseRewind: &off, UseFileHist: &off, UseReadResults: &off,
	})
	if len(gitOnly) != 1 || gitOnly[0].Source != SeedGitCommit || string(gitOnly[0].Body) != "from-git\n" {
		t.Fatalf("git when worktree off: %+v %q", gitOnly, bodyOf(gitOnly))
	}
}

func TestDisasterGitCommitAtT0NotLater(t *testing.T) {
	cwd := t.TempDir()
	rel := "versioned.txt"
	abs := filepath.Join(cwd, rel)
	tEarly := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tOp := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	gitInit(t, cwd)
	writeRel(t, cwd, rel, "v1\n")
	gitCommit(t, cwd, "v1", tEarly)
	writeRel(t, cwd, rel, "v2\n")
	gitCommit(t, cwd, "v2", tLate)
	_ = os.Remove(abs) // force git seed

	ops := []Op{{
		Timestamp: tOp,
		SessionID: "t0",
		Path:      abs,
		CWD:       cwd,
		Repo:      "acme/t0",
		Kind:      KindPatch,
		OldString: "v1",
		NewString: "v1x",
	}}
	off, on := false, true
	seeds := resolveWith(t, ops, SeedConfig{
		UseWorkTree: &off, UseGitCommit: &on, UseGitStash: &off, UseRewind: &off, UseFileHist: &off, UseReadResults: &off,
	})
	if len(seeds) != 1 || string(seeds[0].Body) != "v1\n" {
		t.Fatalf("want pre-image at/before op time (v1), got %q detail=%s", bodyOf(seeds), detailOf(seeds))
	}
}

func TestDisasterWorktreeTooNewRefused(t *testing.T) {
	cwd := t.TempDir()
	rel := "stale.txt"
	abs := filepath.Join(cwd, rel)
	t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	writeRel(t, cwd, rel, "rebuilt-later\n")
	_ = os.Chtimes(abs, t0.Add(2*time.Hour), t0.Add(2*time.Hour))

	ops := []Op{{
		Timestamp: t0,
		Path:      abs,
		CWD:       cwd,
		Repo:      "acme/stale",
		Kind:      KindPatch,
		OldString: "a",
		NewString: "b",
	}}
	off, on := false, true
	seeds := resolveWith(t, ops, SeedConfig{
		UseWorkTree: &on, UseGitCommit: &off, UseGitStash: &off, UseRewind: &off, UseFileHist: &off, UseReadResults: &off,
	})
	if len(seeds) != 0 {
		t.Fatalf("worktree mtime >> t0 must be refused, got %q", bodyOf(seeds))
	}
}

func TestDisasterDryRunParity(t *testing.T) {
	ds := plantDisaster(t)
	ops := patchOnlyTape(ds)
	seeds := seedsFromDir(ds.cwd, ops)

	dry := DefaultConfig()
	dry.DryRun = true
	dryRoot := t.TempDir()
	dryRep, err := NewEngine(dry).Run(dryRoot, ops, seeds...)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dryRoot)
	if len(entries) != 0 {
		t.Fatal("dry-run must not write quarantine files")
	}

	liveRoot := t.TempDir()
	liveRep, err := NewEngine(DefaultConfig()).Run(liveRoot, ops, seeds...)
	if err != nil {
		t.Fatal(err)
	}
	if dryRep.OpsApplied != liveRep.OpsApplied || dryRep.OpsSkipped != liveRep.OpsSkipped || dryRep.OpsFailed != liveRep.OpsFailed {
		t.Fatalf("dry (%d/%d/%d) != live (%d/%d/%d)",
			dryRep.OpsApplied, dryRep.OpsSkipped, dryRep.OpsFailed,
			liveRep.OpsApplied, liveRep.OpsSkipped, liveRep.OpsFailed)
	}
	if len(dryRep.Results) != len(liveRep.Results) {
		t.Fatal("result length mismatch")
	}
	for i := range dryRep.Results {
		if dryRep.Results[i].Outcome != liveRep.Results[i].Outcome || dryRep.Results[i].Reason != liveRep.Results[i].Reason {
			t.Fatalf("result[%d] dry=%s/%s live=%s/%s", i,
				dryRep.Results[i].Outcome, dryRep.Results[i].Reason,
				liveRep.Results[i].Outcome, liveRep.Results[i].Reason)
		}
	}
}

func TestDisasterReplayStable(t *testing.T) {
	ds := plantDisaster(t)
	ops := patchOnlyTape(ds)
	seeds := seedsFromDir(ds.cwd, ops)

	aRoot, bRoot := t.TempDir(), t.TempDir()
	a, err := NewEngine(DefaultConfig()).Run(aRoot, ops, seeds...)
	if err != nil {
		t.Fatal(err)
	}
	// Permute op slice order; SortOps inside Run must stabilise.
	shuffled := append([]Op(nil), ops...)
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	b, err := NewEngine(DefaultConfig()).Run(bRoot, shuffled, seeds...)
	if err != nil {
		t.Fatal(err)
	}
	assertQuarantineEqual(t, aRoot, a, bRoot, b)
}

func TestDisasterJevonsMiniRewindGolden(t *testing.T) {
	// Miniaturised jevons-shaped corpse: edit-heavy tape, crown jewel only in rewind.
	cwd := t.TempDir()
	sid := "01a013f8-670b-7df2-8abd-537e2230b1f3"
	tSnap := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	tOp := tSnap.Add(2 * time.Hour)
	rel := "ui/src/cockpit.css"
	body := "/* jevons mini */\n.cockpit { display: grid; }\n"
	patched := "/* jevons mini */\n.cockpit { display: flex; }\n"

	gitInit(t, cwd) // repo layout for quarantine keys; file never committed
	grokHome := writeGrokRewind(t, sid, cwd, map[string]string{rel: body}, tSnap)

	abs := filepath.Join(cwd, filepath.FromSlash(rel))
	ops := []Op{{
		Timestamp: tOp,
		SessionID: sid,
		Source:    "grok",
		ToolUseID: "sr1",
		Path:      abs,
		CWD:       cwd,
		Repo:      "marcelocantos/jevons",
		Kind:      KindPatch,
		OldString: "display: grid",
		NewString: "display: flex",
	}}

	off, on := false, true
	seeds := resolveWith(t, ops, SeedConfig{
		GrokHome:       grokHome,
		UseWorkTree:    &off,
		UseGitCommit:   &off,
		UseGitStash:    &off,
		UseRewind:      &on,
		UseFileHist:    &off,
		UseReadResults: &off,
	})
	if len(seeds) != 1 || seeds[0].Source != SeedGrokRewind {
		t.Fatalf("want grok_rewind seed, got %+v", seeds)
	}
	if string(seeds[0].Body) != body {
		t.Fatalf("rewind body mismatch: %q", seeds[0].Body)
	}

	root := t.TempDir()
	rep, err := NewEngine(DefaultConfig()).Run(root, ops, seeds...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OpsApplied != 1 || rep.OpsSkipped != 0 {
		t.Fatalf("applied=%d skipped=%d", rep.OpsApplied, rep.OpsSkipped)
	}
	got := quarantineBodies(t, root, rep)
	var css []byte
	for k, v := range got {
		if strings.HasSuffix(k, "ui/src/cockpit.css") {
			css = v
			break
		}
	}
	if string(css) != patched {
		t.Fatalf("cockpit.css=%q want %q", css, patched)
	}
}

func TestDisasterOracleCatchesWrongSeed(t *testing.T) {
	// Mutation drill: twin comparison must fail if we plant a wrong pre-image.
	ds := plantDisaster(t)
	ops := patchOnlyTape(ds)
	good := seedsFromDir(ds.cwd, ops)
	bad := append([]Seed(nil), good...)
	if len(bad) == 0 {
		t.Fatal("no seeds")
	}
	bad[0].Body = []byte("CORRUPT PREIMAGE\n")

	goodRoot, badRoot := t.TempDir(), t.TempDir()
	goodRep, err := NewEngine(DefaultConfig()).Run(goodRoot, ops, good...)
	if err != nil {
		t.Fatal(err)
	}
	badRep, err := NewEngine(DefaultConfig()).Run(badRoot, ops, bad...)
	if err != nil {
		t.Fatal(err)
	}
	g := quarantineBodies(t, goodRoot, goodRep)
	b := quarantineBodies(t, badRoot, badRep)
	for k, gv := range g {
		if !bytes.Equal(gv, b[k]) {
			return // oracle distinguishes — good
		}
	}
	t.Fatal("oracle weak: corrupt seed produced identical quarantine")
}

// --- disaster plant / helpers ---

type disaster struct {
	cwd  string
	repo string
	sid  string
	t0   map[string]string
	at   time.Time
}

func plantDisaster(t *testing.T) disaster {
	t.Helper()
	cwd := t.TempDir()
	at := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	files := map[string]string{
		"README.md":          "# disaster plant\n",
		"ui/src/cockpit.css": ".root { color: navy; }\n",
		"ui/src/App.tsx":     "export const App = () => null;\n",
	}
	gitInit(t, cwd)
	for rel, body := range files {
		writeRel(t, cwd, rel, body)
	}
	gitCommit(t, cwd, "T0", at.Add(-time.Hour))
	return disaster{
		cwd:  cwd,
		repo: "acme/disaster",
		sid:  "disaster-sess",
		t0:   files,
		at:   at,
	}
}

func patchOnlyTape(ds disaster) []Op {
	var ops []Op
	i := 0
	for rel, body := range ds.t0 {
		i++
		line := strings.TrimSpace(strings.Split(body, "\n")[0])
		ops = append(ops, Op{
			Timestamp: ds.at.Add(time.Duration(i) * time.Minute),
			SessionID: ds.sid,
			Source:    "grok",
			ToolUseID: "p" + filepath.Base(rel),
			Path:      filepath.Join(ds.cwd, filepath.FromSlash(rel)),
			CWD:       ds.cwd,
			Repo:      ds.repo,
			Kind:      KindPatch,
			OldString: line,
			NewString: line + " /*patched*/",
		})
	}
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].Path < ops[j].Path
	})
	return ops
}

func seedsFromDir(dir string, ops []Op) []Seed {
	seeds, _ := ResolveSeeds(nil, ops, SeedConfig{SeedFrom: dir})
	return seeds
}

func resolveWith(t *testing.T, ops []Op, cfg SeedConfig) []Seed {
	t.Helper()
	seeds, _ := ResolveSeeds(nil, ops, cfg)
	return seeds
}

func sourcesByPath(seeds []Seed) map[string]SeedSource {
	out := make(map[string]SeedSource)
	for _, s := range seeds {
		out[s.AbsPath] = s.Source
	}
	return out
}

func bodyOf(seeds []Seed) string {
	if len(seeds) == 0 {
		return ""
	}
	return string(seeds[0].Body)
}

func detailOf(seeds []Seed) string {
	if len(seeds) == 0 {
		return ""
	}
	return seeds[0].Detail
}

func assertReportsMatchOnRecovered(t *testing.T, twin *Report, twinRoot string, rec *Report, recRoot string) {
	t.Helper()
	tw := quarantineBodies(t, twinRoot, twin)
	rc := quarantineBodies(t, recRoot, rec)
	if len(tw) == 0 {
		t.Fatal("twin wrote nothing")
	}
	for k, want := range tw {
		got, ok := rc[k]
		if !ok {
			t.Fatalf("recovered missing key %s", k)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("key %s twin=%q recovered=%q", k, want, got)
		}
	}
}

func assertQuarantineEqual(t *testing.T, aRoot string, a *Report, bRoot string, b *Report) {
	t.Helper()
	aa := quarantineBodies(t, aRoot, a)
	bb := quarantineBodies(t, bRoot, b)
	if len(aa) != len(bb) {
		t.Fatalf("quarantine size %d vs %d", len(aa), len(bb))
	}
	for k, v := range aa {
		if !bytes.Equal(v, bb[k]) {
			t.Fatalf("%s differs", k)
		}
	}
}

func quarantineBodies(t *testing.T, root string, rep *Report) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	for key := range rep.FilesWritten {
		full, err := QuarantinePath(root, key)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		out[key] = b
	}
	return out
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "replay@test")
	runGit(t, dir, "config", "user.name", "replay")
}

func gitCommit(t *testing.T, dir, msg string, when time.Time) {
	t.Helper()
	runGit(t, dir, "add", "-A")
	env := append(os.Environ(),
		"GIT_AUTHOR_DATE="+when.Format(time.RFC3339),
		"GIT_COMMITTER_DATE="+when.Format(time.RFC3339),
	)
	cmd := exec.Command("git", "-C", dir, "commit", "-m", msg)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeRel(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGrokRewind(t *testing.T, sessionID, cwd string, files map[string]string, captured time.Time) string {
	t.Helper()
	home := t.TempDir()
	enc := "test-cwd"
	dir := filepath.Join(home, "sessions", enc, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	snaps := make(map[string]rewindSnap)
	for rel, body := range files {
		snaps[rel] = rewindSnap{
			Path:       rel,
			Content:    body,
			CapturedAt: captured.Format(time.RFC3339Nano),
		}
	}
	rec := map[string]any{
		"created_at":     captured.Format(time.RFC3339Nano),
		"file_snapshots": snaps,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rewind_points.jsonl"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = cwd
	return home
}
