// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Resolving a loose reference to a session (🎯T115 / 🎯T125).
//
// The premise is that people know what a conversation was about, or where
// it happened, but not its UUID — "the mnemo session about the WAL", "latest
// squz/yourworld", "that one from yesterday". Requiring an id first turns a
// one-step want into a two-step plan, so one field accepts all the forms
// people actually use and decides by content.

// SessionRef is a resolved session and the context needed to act on it.
type SessionRef struct {
	SessionID string `json:"session_id"`
	Repo      string `json:"repo,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	// Source is the agent that produced the session: claude, codex, grok.
	// Resume invocations differ per source, so callers must branch on it.
	Source  string `json:"source,omitempty"`
	LastMsg string `json:"last_msg,omitempty"`
	Topic   string `json:"topic,omitempty"`
}

// sessionIDShape matches the id forms a session can be named by: a full
// UUID or a hex prefix of one. Deliberately strict — a bare word like
// "mnemo" must fall through to the repo interpretation rather than being
// tried as an id first.
var sessionIDShape = regexp.MustCompile(`^[0-9a-fA-F][0-9a-fA-F-]{5,}$`)

// ResolveSessionRef turns a loose reference into exactly one session.
//
// Accepted forms, in the order they are tried:
//
//	""                    newest session overall
//	"latest" / "recent"   same
//	"latest:<scope>"      newest session in a repo/project matching <scope>
//	"latest <scope>"      same, as people actually speak it
//	"<id or prefix>"      that session; an exact id always wins
//	"<repo fragment>"     newest session whose repo/project matches
//
// An id prefix matching several sessions is an error listing the
// candidates rather than an arbitrary pick — the one case where guessing
// would silently open the wrong conversation. A repo fragment matching
// several is not ambiguous: newest wins, which is what "resume that
// project" means.
func (s *Store) ResolveSessionRef(ref string) (SessionRef, error) {
	ref = strings.TrimSpace(ref)

	// "latest", "recent", "latest:foo", "latest foo".
	if scope, ok := parseLatest(ref); ok {
		return s.newestSession(scope)
	}
	if ref == "" {
		return s.newestSession("")
	}

	// An id or prefix. Exact match wins outright; a prefix must be unique.
	if sessionIDShape.MatchString(ref) {
		if hit, err := s.sessionByID(ref); err == nil {
			return hit, nil
		} else if !isNoSessionMatch(err) {
			return SessionRef{}, err
		}
		// Not an id after all (e.g. a repo that looks hex-ish) — fall
		// through to the repo interpretation rather than failing.
	}

	hit, err := s.newestSession(ref)
	if err != nil && isNoSessionMatch(err) {
		return SessionRef{}, fmt.Errorf(
			"no session matches %q — try a repo name, a session id, or \"latest\"", ref)
	}
	return hit, err
}

// parseLatest recognises the recency shorthands and any scope attached to
// them, returning the scope (possibly empty) and whether it matched.
func parseLatest(ref string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(ref))
	for _, kw := range []string{"latest", "recent"} {
		if lower == kw {
			return "", true
		}
		for _, sep := range []string{":", " "} {
			if rest, ok := strings.CutPrefix(lower, kw+sep); ok {
				return strings.TrimSpace(rest), true
			}
		}
	}
	return "", false
}

// errNoSessionMatch marks "nothing matched" so callers can distinguish it
// from a real failure and fall through to another interpretation.
var errNoSessionMatch = fmt.Errorf("no matching session")

func isNoSessionMatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), errNoSessionMatch.Error())
}

const sessionRefCols = `sm.session_id, COALESCE(sm.repo,''), COALESCE(sm.cwd,''),
	COALESCE(sm.source,'claude'), COALESCE(ss.last_msg,''), COALESCE(sm.topic,'')`

// sessionByID resolves an exact id, or a unique prefix of one.
func (s *Store) sessionByID(ref string) (SessionRef, error) {
	var out SessionRef
	err := s.readDB.QueryRow(`
		SELECT `+sessionRefCols+`
		FROM session_meta sm
		LEFT JOIN session_summary ss ON ss.session_id = sm.session_id
		WHERE sm.session_id = ?
	`, ref).Scan(&out.SessionID, &out.Repo, &out.CWD, &out.Source, &out.LastMsg, &out.Topic)
	if err == nil {
		return out, nil
	}

	rows, err := s.readDB.Query(`
		SELECT `+sessionRefCols+`
		FROM session_meta sm
		LEFT JOIN session_summary ss ON ss.session_id = sm.session_id
		WHERE sm.session_id LIKE ? || '%'
		ORDER BY ss.last_msg DESC
		LIMIT 6
	`, ref)
	if err != nil {
		return SessionRef{}, err
	}
	defer rows.Close()
	var hits []SessionRef
	for rows.Next() {
		var r SessionRef
		if err := rows.Scan(&r.SessionID, &r.Repo, &r.CWD, &r.Source, &r.LastMsg, &r.Topic); err != nil {
			return SessionRef{}, err
		}
		hits = append(hits, r)
	}
	switch len(hits) {
	case 0:
		return SessionRef{}, fmt.Errorf("%w for id %q", errNoSessionMatch, ref)
	case 1:
		return hits[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "id prefix %q matches %d sessions:", ref, len(hits))
		for _, h := range hits {
			fmt.Fprintf(&b, "\n  %s  %s  %s", h.SessionID, h.Repo, h.LastMsg)
		}
		b.WriteString("\nuse a longer prefix")
		return SessionRef{}, fmt.Errorf("%s", b.String())
	}
}

// newestSession returns the most recently active session, optionally
// restricted to a repo/project matching scope. Substantive sessions only —
// resuming an empty one-message session is never what was meant.
func (s *Store) newestSession(scope string) (SessionRef, error) {
	q := `
		SELECT ` + sessionRefCols + `
		FROM session_meta sm
		JOIN session_summary ss ON ss.session_id = sm.session_id
		WHERE ss.substantive_msgs >= 2
		  AND (? = '' OR sm.repo LIKE '%' || ? || '%' OR ss.project LIKE '%' || ? || '%')
		ORDER BY ss.last_msg DESC
		LIMIT 1`
	var out SessionRef
	err := s.readDB.QueryRow(q, scope, scope, scope).Scan(
		&out.SessionID, &out.Repo, &out.CWD, &out.Source, &out.LastMsg, &out.Topic)
	if err != nil {
		if scope == "" {
			return SessionRef{}, fmt.Errorf("%w: the index has no sessions yet", errNoSessionMatch)
		}
		return SessionRef{}, fmt.Errorf("%w for %q", errNoSessionMatch, scope)
	}
	return out, nil
}

// ResumeCommand returns the shell command that reopens this session in its
// own agent, or an error naming the source when there is no known way.
//
// mnemo indexes three agents and they do not share a CLI, so this must not
// guess: opening a bare shell for a Codex session would look like success
// while silently doing something else.
func (r SessionRef) ResumeCommand() (string, error) {
	switch strings.ToLower(strings.TrimSpace(r.Source)) {
	case "", "claude":
		return "claude --resume " + r.SessionID, nil
	default:
		return "", fmt.Errorf(
			"no known resume command for %s sessions — mnemo can reopen Claude Code sessions today; "+
				"this one is at %s", r.Source, r.CWD)
	}
}
