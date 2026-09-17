// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teampush

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// ExcludeFile, placed at a repo root, opts every session in that repo
// out of push. The design's proactive escape hatch, for repos holding
// proprietary client work or anything else that must not leave the
// machine.
//
// It is checked on every push, not cached, and its presence is
// absolute: no flag overrides it. A file whose protection can be
// overridden by a command-line argument protects nothing, because the
// argument is exactly what a hurried contributor reaches for.
const ExcludeFile = ".mnemo-push-exclude"

// PrivateMarker in a repo's CLAUDE.md excludes that repo the same way,
// for contributors who would rather keep the declaration with the
// project's other instructions than add a dotfile.
const PrivateMarker = "mnemo: private"

// LocalIndex is the slice of the local store the collector reads.
type LocalIndex interface {
	TeamPushCandidates(f store.TeamPushFilter) ([]store.TeamPushCandidate, error)
	TeamPushMessages(sessionID string) ([]store.TeamPushMessage, error)
}

// Collected is one session prepared for push, plus what was dropped on
// the way. The second half is the point: a push summary that reports
// only what was sent gives a contributor no way to notice that their
// privacy rules did or did not do what they expected.
type Collected struct {
	Session Session

	// SkippedReason, when non-empty, means this session will NOT be
	// pushed and says why.
	SkippedReason string

	// Candidate is the local metadata the session came from. Retained
	// for --dry-run output; never transmitted.
	Candidate store.TeamPushCandidate
}

// CollectOptions configures a collection pass.
type CollectOptions struct {
	Filter   store.TeamPushFilter
	Redactor *Redactor
}

// Collect reads matching local sessions, applies the opt-out rules and
// the redaction pipeline, and returns push-ready payloads.
//
// Order is deliberate and worth stating: exclusion is decided BEFORE
// any content is read, so an excluded repo's text is never loaded, let
// alone redacted-and-discarded. Redaction then runs over everything
// that survives, and the result is what the caller may transmit. There
// is no path in this package from a local message to the wire that does
// not pass through the redactor.
func Collect(idx LocalIndex, opts CollectOptions) ([]Collected, error) {
	if opts.Redactor == nil {
		// A nil redactor would produce a working push of unredacted
		// transcripts, which is the single worst failure this package
		// could have. Refuse rather than default.
		return nil, fmt.Errorf("collect: no redactor configured; refusing to prepare an unredacted push")
	}
	candidates, err := idx.TeamPushCandidates(opts.Filter)
	if err != nil {
		return nil, err
	}

	excluded := map[string]string{}
	var out []Collected
	for _, c := range candidates {
		col := Collected{Candidate: c}

		root := repoRootFor(c.Cwd)
		reason, known := excluded[root]
		if !known {
			reason = exclusionReason(root)
			excluded[root] = reason
		}
		if reason != "" {
			col.SkippedReason = reason
			out = append(out, col)
			continue
		}

		msgs, err := idx.TeamPushMessages(c.SessionID)
		if err != nil {
			return nil, fmt.Errorf("read session %s: %w", c.SessionID, err)
		}
		if len(msgs) == 0 {
			col.SkippedReason = "no pushable content"
			out = append(out, col)
			continue
		}

		sess := Session{
			Version:        ProtocolVersion,
			SessionID:      c.SessionID,
			Source:         c.Source,
			Repo:           c.Repo,
			Project:        c.Project,
			GitBranch:      c.GitBranch,
			WorkType:       c.WorkType,
			Topic:          c.Topic,
			StartedAt:      c.StartedAt,
			EndedAt:        c.EndedAt,
			RedactionTally: Tally{},
		}
		for i, m := range msgs {
			text, tally := opts.Redactor.Redact(m.Text)
			sess.RedactionTally.Merge(tally)
			sess.Entries = append(sess.Entries, Entry{
				Index:       i,
				Role:        m.Role,
				ContentType: m.ContentType,
				Text:        text,
				Timestamp:   m.Timestamp,
				ToolName:    m.ToolName,
				ToolUseID:   m.ToolUseID,
				IsError:     m.IsError,
			})
		}
		// Topic and branch are contributor-authored text and can carry
		// the same material message bodies do — a branch named after an
		// internal client, say.
		sess.Topic, _ = opts.Redactor.Redact(sess.Topic)
		sess.GitBranch, _ = opts.Redactor.Redact(sess.GitBranch)

		col.Session = sess
		out = append(out, col)
	}
	return out, nil
}

// Pushable returns just the sessions that will actually be transmitted.
func Pushable(collected []Collected) []Session {
	var out []Session
	for _, c := range collected {
		if c.SkippedReason == "" {
			out = append(out, c.Session)
		}
	}
	return out
}

// repoRootFor walks up from a session's working directory to the
// enclosing git repo root, falling back to the cwd itself. The exclude
// file lives at the repo root, but sessions commonly run in a
// subdirectory of it.
func repoRootFor(cwd string) string {
	if cwd == "" {
		return ""
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
		dir = parent
	}
}

// exclusionReason reports why a repo root is opted out, or "" if it is
// not.
//
// An unreadable directory counts as EXCLUDED, not included. If we
// cannot tell whether a contributor opted this repo out, the safe
// reading of the ambiguity is the one that does not publish.
func exclusionReason(root string) string {
	if root == "" {
		return ""
	}
	path := filepath.Join(root, ExcludeFile)
	switch _, err := os.Stat(path); {
	case err == nil:
		return ExcludeFile + " at the repo root"
	case !os.IsNotExist(err):
		return fmt.Sprintf("cannot check %s (%v) — excluded to be safe", path, err)
	}

	claudeMD := filepath.Join(root, "CLAUDE.md")
	data, err := os.ReadFile(claudeMD)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("cannot read %s (%v) — excluded to be safe", claudeMD, err)
	}
	if strings.Contains(string(data), PrivateMarker) {
		return "CLAUDE.md carries the '" + PrivateMarker + "' marker"
	}
	return ""
}
