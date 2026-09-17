// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teampush

import (
	"errors"
	"fmt"
	"strings"
)

// PushPath is the HTTP path the central instance serves for pushes.
const PushPath = "/push"

// AuthorHeader carries the contributor's claimed identity (🎯T36.1).
//
// The design chose an out-of-band header over an identity baked into
// the mTLS cert: a cert lives ten years, and a contributor who changes
// email or GitHub handle should not need a new one. The header is not a
// new trust surface because the server accepts a claim only from a
// caller who already completed the mTLS handshake, and validates the
// claim against the trusted-peer store before using it.
const AuthorHeader = "X-Mnemo-Author"

// ProtocolVersion is the payload's wire version. The server rejects a
// version it does not know rather than guessing at the shape, because
// a partially understood transcript is worse than a refused one — it
// lands in the team index looking complete.
const ProtocolVersion = 1

// ContentTypeThinking and ContentTypeImage name the two content kinds
// the design excludes from push entirely. They are declared here rather
// than inline at the filter so the server can reject them too: a client
// that sends them is either an old build or a modified one, and either
// way the team index must not hold them.
const (
	ContentTypeThinking = "thinking"
	ContentTypeImage    = "image"
)

// Session is one pushed session — already filtered and redacted. This
// is the complete wire shape: there is no field carrying original text,
// no raw JSONL passthrough, and no reference the server could follow to
// fetch more. What the contributor's redactor produced is all there is.
type Session struct {
	// Version is ProtocolVersion at the time of the push.
	Version int `json:"version"`

	// SessionID is the Claude Code (or Codex/Grok/Cursor) session UUID.
	// Stable and globally unique, which is what makes it the dedup key.
	SessionID string `json:"session_id"`

	// Source is the agent that produced the transcript — "claude",
	// "codex", "grok", "cursor". Mirrors session_meta.source.
	Source string `json:"source"`

	// Repo is the canonical repo name the session was connected to.
	// Push is repo-scoped, so this is never empty on a valid payload.
	Repo string `json:"repo"`

	// Project is the Claude Code project-directory name.
	Project string `json:"project,omitempty"`

	// GitBranch, WorkType and Topic are session_meta fields that give
	// team-scope queries something to filter and group by.
	GitBranch string `json:"git_branch,omitempty"`
	WorkType  string `json:"work_type,omitempty"`
	Topic     string `json:"topic,omitempty"`

	// StartedAt and EndedAt are RFC3339 timestamps of the first and
	// last pushed entry.
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`

	// Entries are the filtered, redacted messages in transcript order.
	Entries []Entry `json:"entries"`

	// RedactionTally reports what the contributor's pipeline removed,
	// by rule name. Sent deliberately: the team index can then show
	// "3 secrets redacted" beside a session, so a reader knows the
	// transcript is incomplete by design rather than by truncation.
	RedactionTally Tally `json:"redaction_tally,omitempty"`

	// Cwd is deliberately absent. It is a local filesystem path, it
	// identifies the contributor's machine layout, and no team-scope
	// query needs it — repo is the dimension people search by.
}

// Entry is one message within a pushed session. The shape mirrors the
// flattened content-block model mnemo already stores in `messages`, so
// the server writes what it receives without re-deriving anything from
// a raw transcript line it was never sent.
type Entry struct {
	// Index is the entry's position within the session, assigned by the
	// contributor at push time. The team index keys entries by
	// (session_id, index) rather than by UUID: the design calls for
	// cross-contributor UUID collisions to be harmless, and a positional
	// key makes them so without trusting UUID uniqueness.
	Index int `json:"index"`

	// Role is "user" or "assistant".
	Role string `json:"role"`

	// ContentType is "text", "tool_use" or "tool_result". Never
	// "thinking" or "image" — those are filtered before transmission
	// and rejected on receipt.
	ContentType string `json:"content_type"`

	// Text is the redacted content.
	Text string `json:"text"`

	// Timestamp is RFC3339.
	Timestamp string `json:"timestamp,omitempty"`

	// ToolName and ToolUseID carry tool identity for tool_use and
	// tool_result entries.
	ToolName  string `json:"tool_name,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`

	// IsError marks a failed tool result.
	IsError bool `json:"is_error,omitempty"`
}

// PushRequest is the body of POST /push. Sessions are batched so a
// contributor pushing a week of work makes one request, not fifty.
type PushRequest struct {
	Version  int       `json:"version"`
	Sessions []Session `json:"sessions"`
}

// SessionOutcome is what happened to one session in a push.
type SessionOutcome struct {
	SessionID string `json:"session_id"`

	// Status is one of StatusAccepted, StatusSkipped, StatusConflict.
	Status string `json:"status"`

	// Accepted counts entries newly written. A re-push of a session
	// that has grown reports Status accepted with only the new entries
	// counted here.
	Accepted int `json:"accepted"`

	// Reason explains a skip or conflict in one line, for the
	// contributor's summary output.
	Reason string `json:"reason,omitempty"`
}

// Push outcome statuses.
const (
	// StatusAccepted: entries were written (a first push, or a re-push
	// that appended).
	StatusAccepted = "accepted"

	// StatusSkipped: the session is already present with nothing new.
	StatusSkipped = "skipped"

	// StatusConflict: the session UUID exists under a different author.
	// Never overwritten — see the design's containment rules.
	StatusConflict = "conflict"
)

// PushResponse is the server's structured reply.
type PushResponse struct {
	// Author is the identity the server attributed the push to, after
	// validating the claim. Echoed back so a contributor can catch a
	// misconfigured X-Mnemo-Author immediately rather than discovering
	// weeks of work filed under the wrong name.
	Author string `json:"author"`

	Accepted  int `json:"accepted"`
	Skipped   int `json:"skipped"`
	Conflicts int `json:"conflicts"`

	// Entries is the total number of entry rows written.
	Entries int `json:"entries"`

	Sessions []SessionOutcome `json:"sessions"`
}

// ErrEmptyPush is returned when a payload carries no sessions.
var ErrEmptyPush = errors.New("push carries no sessions")

// Validate checks a payload for the invariants the server relies on,
// and is called on BOTH sides. The client calls it so a malformed push
// fails locally with a clear message; the server calls it because a
// client-side check binds only clients that choose to run it.
func (r *PushRequest) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("unsupported push protocol version %d (this server speaks %d)",
			r.Version, ProtocolVersion)
	}
	if len(r.Sessions) == 0 {
		return ErrEmptyPush
	}
	seen := map[string]int{}
	for i := range r.Sessions {
		s := &r.Sessions[i]
		if err := s.validate(); err != nil {
			return fmt.Errorf("sessions[%d]: %w", i, err)
		}
		if prev, dup := seen[s.SessionID]; dup {
			return fmt.Errorf("sessions[%d]: session %s also appears at index %d",
				i, s.SessionID, prev)
		}
		seen[s.SessionID] = i
	}
	return nil
}

func (s *Session) validate() error {
	if strings.TrimSpace(s.SessionID) == "" {
		return errors.New("session_id is required")
	}
	if strings.TrimSpace(s.Repo) == "" {
		// Push is repo-scoped by design. A session with no repo has no
		// team-scope meaning and would be unreachable by every query
		// the team tools offer, so refusing it here is kinder than
		// storing content nobody can find.
		return fmt.Errorf("session %s: repo is required (push is repo-scoped)", s.SessionID)
	}
	if len(s.Entries) == 0 {
		return fmt.Errorf("session %s: no entries", s.SessionID)
	}
	for i, e := range s.Entries {
		switch e.ContentType {
		case ContentTypeThinking, ContentTypeImage:
			return fmt.Errorf("session %s entry %d: content_type %q is never pushed",
				s.SessionID, i, e.ContentType)
		}
		if e.Index < 0 {
			return fmt.Errorf("session %s entry %d: negative index", s.SessionID, i)
		}
	}
	return nil
}
