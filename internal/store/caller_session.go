// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// callerSessionShape matches the ids mnemo stores as session_id. A
// short token must not exclude a session by accident.
var callerSessionShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)

// Bounds for resolving a tool-use id that ingest has not indexed yet.
// The walk stats session files; only the newest few within the window
// are read, and only their tails. The calling file is the one Claude
// just appended, so the common case reads a single tail.
const (
	callerDBRetries  = 2
	callerDBRetryGap = 15 * time.Millisecond
	callerScanLimit  = 8
	callerScanWindow = 3 * time.Minute
	callerTailBytes  = 256 << 10
)

// callerAfterDBMiss, when set, runs after a database miss and before
// each retry. Tests use it to land the row between attempts.
var callerAfterDBMiss func()

func (s *Store) resolveToolUseSession(toolUseID string) (string, error) {
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		return "", nil
	}
	if sid, ok := s.cachedCaller(toolUseID); ok {
		return sid, nil
	}
	sid, err := s.sessionForToolUseDB(toolUseID)
	if err != nil || sid != "" {
		if sid != "" {
			s.rememberCaller(toolUseID, sid)
		}
		return sid, err
	}
	for range callerDBRetries {
		if callerAfterDBMiss != nil {
			callerAfterDBMiss()
		}
		time.Sleep(callerDBRetryGap)
		sid, err = s.sessionForToolUseDB(toolUseID)
		if err != nil || sid != "" {
			if sid != "" {
				s.rememberCaller(toolUseID, sid)
			}
			return sid, err
		}
	}
	sid = s.sessionFromTranscripts(toolUseID)
	if sid != "" {
		s.rememberCaller(toolUseID, sid)
	}
	return sid, nil
}

func (s *Store) cachedCaller(toolUseID string) (string, bool) {
	s.callerCacheMu.Lock()
	defer s.callerCacheMu.Unlock()
	if s.callerCache == nil {
		return "", false
	}
	sid, ok := s.callerCache[toolUseID]
	return sid, ok
}

func (s *Store) rememberCaller(toolUseID, sessionID string) {
	s.callerCacheMu.Lock()
	defer s.callerCacheMu.Unlock()
	if s.callerCache == nil {
		s.callerCache = map[string]string{}
	}
	s.callerCache[toolUseID] = sessionID
}

func (s *Store) sessionForToolUseDB(toolUseID string) (string, error) {
	var sid string
	err := s.readDB.QueryRow(
		`SELECT session_id FROM messages WHERE tool_use_id = ? AND session_id != '' LIMIT 1`,
		toolUseID).Scan(&sid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sid, err
}

type callerLine struct {
	SessionID string          `json:"sessionId"`
	Message   json.RawMessage `json:"message"`
}

// sessionFromTranscripts scans the newest session JSONLs under the
// configured Claude ingest roots. The session id is the sessionId on
// the matching entry when that field is present, otherwise the file
// stem, which is what ingest stores.
func (s *Store) sessionFromTranscripts(toolUseID string) string {
	files := s.recentSessionFiles(time.Now())
	for _, f := range files {
		if sid := sessionInTail(f.path, toolUseID); sid != "" {
			return sid
		}
	}
	return ""
}

type callerFile struct {
	path  string
	mtime time.Time
}

func (s *Store) recentSessionFiles(now time.Time) []callerFile {
	cutoff := now.Add(-callerScanWindow)
	var best []callerFile
	consider := func(path string, info os.FileInfo) {
		if info == nil || info.IsDir() {
			return
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".jsonl") || skipSessionName(name) {
			return
		}
		if info.ModTime().Before(cutoff) {
			return
		}
		best = append(best, callerFile{path: path, mtime: info.ModTime()})
	}
	for _, root := range s.projectDirs() {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, p := range projects {
			if !p.IsDir() {
				if info, err := p.Info(); err == nil {
					consider(filepath.Join(root, p.Name()), info)
				}
				continue
			}
			if skipSessionDir(p.Name()) {
				continue
			}
			proj := filepath.Join(root, p.Name())
			entries, err := os.ReadDir(proj)
			if err != nil {
				continue
			}
			for _, e := range entries {
				einfo, err := e.Info()
				if err != nil {
					continue
				}
				if !e.IsDir() {
					consider(filepath.Join(proj, e.Name()), einfo)
					continue
				}
				if skipSessionDir(e.Name()) {
					continue
				}
				// One more level covers <session>/subagents/<id>.jsonl.
				sub := filepath.Join(proj, e.Name())
				kids, err := os.ReadDir(sub)
				if err != nil {
					continue
				}
				for _, k := range kids {
					kinfo, err := k.Info()
					if err != nil {
						continue
					}
					if !k.IsDir() {
						consider(filepath.Join(sub, k.Name()), kinfo)
						continue
					}
					if k.Name() != "subagents" {
						continue
					}
					agents, err := os.ReadDir(filepath.Join(sub, k.Name()))
					if err != nil {
						continue
					}
					for _, a := range agents {
						ainfo, err := a.Info()
						if err != nil || a.IsDir() {
							continue
						}
						consider(filepath.Join(sub, k.Name(), a.Name()), ainfo)
					}
				}
			}
		}
	}
	sort.Slice(best, func(i, j int) bool {
		return best[i].mtime.After(best[j].mtime)
	})
	if len(best) > callerScanLimit {
		best = best[:callerScanLimit]
	}
	return best
}

func skipSessionDir(name string) bool {
	switch name {
	case "tool-results", "memory", "node_modules", ".git", "terminal":
		return true
	default:
		return false
	}
}

func skipSessionName(name string) bool {
	switch name {
	case "chat_history.jsonl", "events.jsonl", "rewind_points.jsonl",
		"prompt_history.jsonl", "hunk_records.jsonl", "feedback.jsonl",
		"updates.jsonl":
		return true
	default:
		return false
	}
}

func sessionInTail(path, toolUseID string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start := int64(0)
	if st.Size() > callerTailBytes {
		start = st.Size() - callerTailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	r := bufio.NewReader(f)
	if start > 0 {
		// The first read is the tail of a line we did not begin.
		if _, err := r.ReadBytes('\n'); err != nil {
			return ""
		}
	}
	needle := []byte(`"` + toolUseID + `"`)
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, needle) {
			if sid := sessionOnLine(line, toolUseID, stem); sid != "" {
				return sid
			}
		}
		if err != nil {
			return ""
		}
	}
}

func sessionOnLine(line []byte, toolUseID, stem string) string {
	var entry callerLine
	if json.Unmarshal(line, &entry) != nil {
		return ""
	}
	for _, b := range extractBlocks(entry.Message) {
		if b.ToolUseID == toolUseID && (b.ContentType == "tool_use" || b.ContentType == "tool_result") {
			if callerSessionShape.MatchString(entry.SessionID) {
				return entry.SessionID
			}
			if callerSessionShape.MatchString(stem) {
				return stem
			}
			return ""
		}
	}
	return ""
}
