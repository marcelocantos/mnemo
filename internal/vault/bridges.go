// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// errDuplicateFence signals that an anchor file already contains two or
// more blocks with the same bridge name — an ambiguous state the writer
// refuses to resolve, leaving the file untouched (🎯T64.6 edge case).
var errDuplicateFence = errors.New("duplicate bridge fence")

// bridgeStartDelim / bridgeEndDelim are the HTML-comment sentinels that
// fence a bridge's managed block inside a user-owned anchor file. The
// writer locates blocks by these delimiters — never by line offset — so
// a user may freely relocate the block within the file (🎯T64.6).
func bridgeStartDelim(name string) string { return "<!-- mnemo:bridge:" + name + " -->" }
func bridgeEndDelim(name string) string   { return "<!-- /mnemo:bridge:" + name + " -->" }

// syncBridges reconciles the configured bridges against the anchor files
// on disk and the written-bridge record in st (🎯T64.6). It:
//
//   - strips blocks for bridges that were previously written but have
//     since been removed from config (or whose collection is now
//     unknown), using st.WrittenBridges to find the anchor file;
//   - relocates a bridge whose anchor path changed (strip old, write new);
//   - writes/updates the block for each currently-configured, recognised
//     bridge;
//   - records per-bridge failures in st.BridgeErrors and the surviving
//     name→anchor map in st.WrittenBridges.
//
// It never returns an error: bridges are auxiliary and a single bad
// anchor must not fail the sync. All problems are surfaced via
// st.BridgeErrors (→ mnemo_vault_status / mnemo_vault_bridge_list) and
// warn-level logs. st is mutated in place; the caller persists it.
//
// writeAllowed is false under a pure-v1 layout: bridge targets are v2
// wing paths that don't exist there, so no block is written — but the
// strip pass still runs, so dropping back to v1 removes any block a
// prior v2 sync left in a user's anchor rather than orphaning it.
func (e *Exporter) syncBridges(st *store.State, writeAllowed bool) {
	var errs []store.BridgeError
	fail := func(name, anchor, reason, detail string) {
		errs = append(errs, store.BridgeError{Name: name, AnchorPath: anchor, Reason: reason, Detail: detail})
	}

	// Current, recognised bridges (skip unknown collections fail-soft).
	// Under a v1 layout nothing is a write target, so current stays empty
	// and the strip pass below reclaims every previously-written block.
	current := map[string]string{}
	if writeAllowed {
		for name, anchor := range e.bridges {
			if !store.IsVaultBridgeCollection(name) {
				fail(name, anchor, "unknown collection", "not one of themes/patterns/cross-repo/lessons/decisions/memories")
				slog.Warn("vault: bridge skipped, unknown collection", "name", name, "anchor", anchor)
				continue
			}
			if strings.TrimSpace(anchor) == "" {
				fail(name, anchor, "empty anchor path", "")
				continue
			}
			// Reject anchors that escape the vault root (e.g. "../x.md").
			// Everything inside the vault is fenced out of ingest via
			// RegisterExcludedPath; a block written outside that boundary
			// would land in mnemo's own doc/todo discovery surface.
			if _, ok := containedAnchor(e.path, anchor); !ok {
				fail(name, anchor, "anchor outside vault", "resolved path escapes vault root")
				slog.Warn("vault: bridge skipped, anchor outside vault", "name", name, "anchor", anchor)
				continue
			}
			current[name] = anchor
		}
	}

	written := st.WrittenBridges
	if written == nil {
		written = map[string]string{}
	}

	// blocked holds names whose relocation strip failed this cycle. Their
	// old-anchor record must survive into st.WrittenBridges and the write
	// below must be skipped, so a later sync retries the strip before
	// moving the block — otherwise the write loop would clobber the
	// retained record and orphan the old block permanently.
	blocked := map[string]bool{}

	// Strip blocks for bridges no longer configured, or whose anchor
	// path changed (strip the old location before writing the new one).
	// Deterministic order so a shared anchor reconciles reproducibly.
	for _, name := range sortedKeys(written) {
		oldAnchor := written[name]
		newAnchor, stillWanted := current[name]
		if stillWanted && newAnchor == oldAnchor {
			continue
		}
		if _, ok := containedAnchor(e.path, oldAnchor); !ok {
			// A record we would never have written; drop it rather than
			// touch a path outside the vault.
			delete(written, name)
			continue
		}
		if err := stripBridgeBlock(filepath.Join(e.path, oldAnchor), name); err != nil {
			slog.Warn("vault: strip removed bridge failed", "name", name, "anchor", oldAnchor, "err", err)
			reason := "strip failed"
			if errors.Is(err, errDuplicateFence) {
				reason = "duplicate fence"
			}
			fail(name, oldAnchor, reason, err.Error())
			// Keep written[name]=oldAnchor so a later sync retries. For a
			// relocation, also skip the write so the new anchor isn't
			// written while the old block still stands.
			if stillWanted {
				blocked[name] = true
			}
			continue
		}
		delete(written, name)
	}

	// Write/update each current bridge, in deterministic order so two
	// bridges sharing one anchor append in a stable sequence.
	for _, name := range sortedKeys(current) {
		if blocked[name] {
			continue
		}
		anchor := current[name]
		body, err := e.renderBridgeBody(name)
		if err != nil {
			fail(name, anchor, "render failed", err.Error())
			slog.Warn("vault: render bridge body failed", "name", name, "err", err)
			continue
		}
		if err := upsertBridgeBlock(filepath.Join(e.path, anchor), name, body); err != nil {
			reason := "write failed"
			if errors.Is(err, errDuplicateFence) {
				reason = "duplicate fence"
			}
			fail(name, anchor, reason, err.Error())
			slog.Warn("vault: write bridge failed", "name", name, "anchor", anchor, "reason", reason, "err", err)
			continue
		}
		written[name] = anchor
	}

	if len(written) == 0 {
		written = nil
	}
	st.WrittenBridges = written
	st.BridgeErrors = errs
	if st.BridgeErrors == nil {
		st.BridgeErrors = []store.BridgeError{}
	}
}

// containedAnchor resolves a vault-relative anchor against root and
// reports whether it stays inside root. Absolute anchors are contained
// by filepath.Join already; only ".." escapes. Returns the joined
// absolute path when contained.
func containedAnchor(root, anchor string) (string, bool) {
	abs := filepath.Join(root, anchor)
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return abs, true
}

// sortedKeys returns a map's keys in ascending order, for deterministic
// iteration over bridge names.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// renderBridgeBody produces the body lines (no fence delimiters) for a
// bridge collection, honouring the profile's link syntax and the
// per-bridge link cap (🎯T64.6). Collections whose pages do not exist
// yet (themes, patterns, cross-repo, lessons — future slices) render an
// empty body; the empty fenced block is written so the bridge lights up
// automatically once those slices land.
func (e *Exporter) renderBridgeBody(name string) ([]string, error) {
	max := e.bridgesMaxLinks
	if max <= 0 {
		max = store.DefaultVaultBridgesMaxLinks
	}
	switch name {
	case "decisions":
		return e.bridgeDecisionLines(max)
	case "memories":
		return e.bridgeMemoryLines(max)
	case "patterns":
		return e.bridgePatternLines(max)
	default:
		// themes / cross-repo / lessons — not materialised yet.
		return nil, nil
	}
}

// bridgeDecisionLines returns a flat, most-recent-first bulleted list of
// links to the wing's decision pages, capped at max. SearchDecisions
// already orders by timestamp DESC at the SQL layer, so passing max as
// the limit yields the most-recent max rows without a client-side sort.
func (e *Exporter) bridgeDecisionLines(max int) ([]string, error) {
	decisions, err := e.backend.SearchDecisions("", "", 36500, max)
	if err != nil {
		return nil, fmt.Errorf("search decisions: %w", err)
	}
	lines := make([]string, 0, len(decisions))
	for _, d := range decisions {
		target := bridgeTarget(decisionPathV2(d))
		alias := "Decision · " + dateOf(d.Timestamp)
		if d.Repo != "" {
			alias += " · " + shortProjectName(d.Repo)
		}
		lines = append(lines, "- "+e.profile.Link(target, alias))
	}
	return lines, nil
}

// bridgePatternLines returns links to the wing's pattern pages, capped
// at max. It queries through the same emission gate syncPatterns uses
// (occurrence >= 3 across >= 2 sessions) — the gate lives in the query,
// not a loop filter — so the bridge, the pages, and the _index hub can
// never disagree about which patterns exist (🎯T64.7).
func (e *Exporter) bridgePatternLines(max int) ([]string, error) {
	patterns, err := e.backend.ListPatterns(store.PatternQuery{
		MinOccurrences: store.PatternEmitMinOccurrences,
		MinSessions:    store.PatternEmitMinSessions,
	})
	if err != nil {
		return nil, fmt.Errorf("list patterns: %w", err)
	}
	lines := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if len(lines) >= max {
			break
		}
		alias := p.PatternType
		if alias == "" {
			alias = "pattern"
		}
		lines = append(lines, "- "+e.profile.Link(bridgeTarget(patternPath(p)), alias))
	}
	return lines, nil
}

// bridgeMemoryLines returns memory links grouped by source project, each
// project a `### <project>` subsection capped at max links, per the
// design's memories-bridge shape.
func (e *Exporter) bridgeMemoryLines(max int) ([]string, error) {
	memories, err := e.backend.SearchMemories("", "", "", 100000)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	byProject := map[string][]store.MemoryInfo{}
	var order []string
	for _, m := range memories {
		proj := shortProjectName(m.Project)
		if proj == "" {
			proj = "unknown"
		}
		if _, seen := byProject[proj]; !seen {
			order = append(order, proj)
		}
		byProject[proj] = append(byProject[proj], m)
	}
	sort.Strings(order)

	var lines []string
	for _, proj := range order {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "### "+proj)
		n := 0
		for _, m := range byProject[proj] {
			if n >= max {
				break
			}
			alias := m.Name
			if alias == "" {
				alias = "memory"
			}
			lines = append(lines, "- "+e.profile.Link(bridgeTarget(memoryPathV2(m)), alias))
			n++
		}
	}
	return lines, nil
}

// bridgeTarget converts a vault-relative note path to the link target
// form expected by Profile.Link: forward slashes, no ".md" suffix.
func bridgeTarget(relPath string) string {
	return strings.TrimSuffix(filepath.ToSlash(relPath), ".md")
}

// upsertBridgeBlock writes the fenced bridge block for name into the
// anchor file at absPath, handling every edge case from the design:
//
//   - anchor missing → create it (with parent dirs) holding a one-line
//     header derived from the filename plus the block;
//   - fence absent → append the block after a blank-line separator;
//   - fence present → replace it in place, located by delimiter;
//   - duplicate fences → return errDuplicateFence, file left untouched;
//   - unwritable anchor → the atomic write returns an error, surfaced up.
func upsertBridgeBlock(absPath, name string, body []string) error {
	block := renderBridgeBlock(name, body)

	// atomicWriteFile writes into a sibling tempfile and does not create
	// parent directories; ensure the anchor's directory exists first.
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("mkdir anchor dir: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if errors.Is(err, os.ErrNotExist) {
		header := "# " + bridgeAnchorTitle(absPath) + "\n\n"
		return atomicWriteFile(absPath, []byte(header+block+"\n"))
	}
	if err != nil {
		return fmt.Errorf("read anchor: %w", err)
	}

	content := string(data)
	start, end, count, err := locateBridgeBlock(content, name)
	if err != nil {
		return err
	}
	var out string
	switch count {
	case 0:
		// Separate the appended block from existing content by exactly one
		// blank line, whatever trailing newlines the file already has.
		sep := "\n\n"
		switch {
		case strings.HasSuffix(content, "\n\n"):
			sep = ""
		case strings.HasSuffix(content, "\n"):
			sep = "\n"
		}
		out = content + sep + block + "\n"
	case 1:
		out = content[:start] + block + content[end:]
	}
	// User-owned files typically sit under Obsidian Sync / iCloud / git;
	// don't rewrite (and break hardlinks / bump mtime) when nothing
	// changed. atomicWriteFile's rename is not free at the sync layer.
	if out == content {
		return nil
	}
	return atomicWriteFile(absPath, []byte(out))
}

// stripBridgeBlock removes the named bridge block from absPath. A missing
// file or absent fence is a no-op. Duplicate fences are left untouched
// (returns errDuplicateFence) so an ambiguous file is never mangled.
func stripBridgeBlock(absPath, name string) error {
	data, err := os.ReadFile(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read anchor: %w", err)
	}
	content := string(data)
	start, end, count, err := locateBridgeBlock(content, name)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	// Drop the block plus one trailing newline and a preceding blank-line
	// separator if present, so removing a bridge doesn't leave a widening
	// gap of blank lines across syncs.
	for end < len(content) && content[end] == '\n' {
		end++
	}
	for start > 0 && content[start-1] == '\n' {
		start--
	}
	out := content[:start] + content[end:]
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return atomicWriteFile(absPath, []byte(out))
}

// renderBridgeBlock assembles the full fenced block text (delimiters +
// body) with no trailing newline.
func renderBridgeBlock(name string, body []string) string {
	var b strings.Builder
	b.WriteString(bridgeStartDelim(name))
	b.WriteString("\n")
	if len(body) > 0 {
		b.WriteString(neutraliseFence(strings.Join(body, "\n")))
		b.WriteString("\n")
	}
	b.WriteString(bridgeEndDelim(name))
	return b.String()
}

// neutraliseFence defuses any HTML-comment delimiter sequences that leak
// into body text (e.g. a memory named "<!-- /mnemo:bridge:memories -->")
// so they can't forge a fence and trip the duplicate-fence guard
// permanently. The replacements render as literal text in Obsidian /
// Markdown. Remote — aliases derive from memory names and repo paths —
// but cheap insurance against a self-inflicted unrecoverable state.
func neutraliseFence(s string) string {
	s = strings.ReplaceAll(s, "<!--", "&lt;!--")
	return strings.ReplaceAll(s, "-->", "--&gt;")
}

// locateBridgeBlock finds the named bridge block within content. It
// returns the byte offset of the start delimiter, the offset just past
// the end delimiter, and the number of well-formed blocks found (0 or
// 1). More than one start delimiter, or a start without a matching end
// after it, yields errDuplicateFence / a malformed error so the caller
// leaves the file alone.
func locateBridgeBlock(content, name string) (start, end, count int, err error) {
	sd, ed := bridgeStartDelim(name), bridgeEndDelim(name)
	if strings.Count(content, sd) > 1 || strings.Count(content, ed) > 1 {
		return 0, 0, 0, errDuplicateFence
	}
	s := strings.Index(content, sd)
	if s < 0 {
		return 0, 0, 0, nil
	}
	eIdx := strings.Index(content[s:], ed)
	if eIdx < 0 {
		return 0, 0, 0, fmt.Errorf("bridge %q: start fence without matching end", name)
	}
	return s, s + eIdx + len(ed), 1, nil
}

// bridgeAnchorTitle derives a human-facing header for a freshly-created
// anchor file from its filename (drops the extension).
func bridgeAnchorTitle(absPath string) string {
	base := filepath.Base(absPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
