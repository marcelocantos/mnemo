// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 🎯T75: transcript-ingest freshness and per-source lag diagnostics.
//
// mnemo_status grew a diagnostic block so an agent can answer "is mnemo
// stale, where, by how much, and which file is behind?" without
// grepping ~/.claude/projects or running ad-hoc SQL. The block is
// additive on StatusResult — existing repos/sessions/streams output is
// untouched. Every scan here is bounded: directory walks + stat only,
// never reading JSONL bodies, with capped examples.

// maxBehindExamples caps the per-source forensic example list.
const maxBehindExamples = 5

// IngestDiagnostics is the freshness/lag block returned by mnemo_status.
type IngestDiagnostics struct {
	// NowUTC is the daemon's wall-clock at report time, so freshness lag
	// is interpretable against a known observation point.
	NowUTC string `json:"now_utc"`
	// Freshness compares now against the freshest indexed content.
	Freshness *FreshnessInfo `json:"freshness,omitempty"`
	// Divergence is the per-stream actual-vs-desired gap (🎯T68.4),
	// including transcript_index pending bytes/files.
	Divergence []StreamDivergence `json:"divergence,omitempty"`
	// Sources is one row per configured Claude project directory.
	Sources []TranscriptSource `json:"transcript_sources,omitempty"`
	// SourceDrift surfaces indexed sources that were deleted, truncated,
	// or rewritten out from under the index (only when non-zero).
	SourceDrift *SourceDriftReport `json:"source_drift,omitempty"`
	// Repo is present only when a repo filter was supplied: it explains
	// that repo's ingest state specifically.
	Repo *RepoDiagnostic `json:"repo_diagnostic,omitempty"`
}

// FreshnessInfo bounds indexer lag: how far behind "now" the freshest
// indexed transcript content is.
type FreshnessInfo struct {
	NewestIndexed string `json:"newest_indexed,omitempty"` // MAX(entries.timestamp), RFC3339
	LagSeconds    int64  `json:"lag_seconds"`
	LagHuman      string `json:"lag_human,omitempty"`
	LastWriteAt   string `json:"last_write_at,omitempty"` // daemon's most recent ingest commit
}

// TranscriptSource is the on-disk vs indexed coverage of one configured
// project directory. All counts come from a bounded walk + stat.
type TranscriptSource struct {
	Path            string       `json:"path"`
	Exists          bool         `json:"exists"`
	Readable        bool         `json:"readable"`
	TotalFiles      int          `json:"total_files"`
	UnknownOffset   int          `json:"unknown_offset_files"` // never ingested (no ingest_state row)
	BehindFiles     int          `json:"behind_files"`         // on-disk size past the ingest offset
	PendingBytes    int64        `json:"pending_bytes"`
	NewestFileMtime string       `json:"newest_file_mtime,omitempty"`
	Examples        []BehindFile `json:"behind_examples,omitempty"`
}

// BehindFile carries enough forensic detail to act on one
// un-converged transcript file.
type BehindFile struct {
	Path          string `json:"path"`
	ProjectDir    string `json:"project_dir"` // basename of the encoded Claude project dir
	SessionID     string `json:"session_id"`  // inferred from the filename
	Size          int64  `json:"size"`
	Offset        int64  `json:"offset"`
	PendingBytes  int64  `json:"pending_bytes"`
	Mtime         string `json:"mtime,omitempty"`
	RecordedMtime string `json:"recorded_mtime,omitempty"`
	// State is one of: new (unseen, no offset row), append_behind
	// (size past offset), truncated (size below offset), rewritten
	// (same size, mtime moved). Deleted/missing sources surface via
	// SourceDrift, not here (they have no on-disk file to walk).
	State string `json:"state"`
}

// RepoDiagnostic explains a single repo's ingest state when mnemo_status
// is called with a repo filter.
type RepoDiagnostic struct {
	RepoFilter         string   `json:"repo_filter"`
	MatchedProjectDirs []string `json:"matched_project_dirs,omitempty"`
	LatestIndexed      string   `json:"latest_indexed,omitempty"`       // newest indexed session activity for the repo
	LatestOnDiskMtime  string   `json:"latest_on_disk_mtime,omitempty"` // newest .jsonl mtime among matched dirs
	Note               string   `json:"note,omitempty"`
}

// IngestDiagnostics assembles the freshness/lag block. repoFilter, when
// non-empty, adds a repo-specific section. Safe for large corpora:
// bounded walks, stat only, capped examples.
func (s *Store) IngestDiagnostics(repoFilter string) *IngestDiagnostics {
	now := time.Now().UTC()
	d := &IngestDiagnostics{NowUTC: now.Format(time.RFC3339)}

	fi := &FreshnessInfo{NewestIndexed: s.newestIndexedTimestamp()}
	if t, err := parseTimestamp(fi.NewestIndexed); err == nil {
		lag := now.Sub(t.UTC())
		if lag < 0 {
			lag = 0
		}
		fi.LagSeconds = int64(lag.Seconds())
		fi.LagHuman = lag.Round(time.Second).String()
	}
	if w := s.LastWriteAt(); !w.IsZero() {
		fi.LastWriteAt = w.UTC().Format(time.RFC3339)
	}
	d.Freshness = fi

	d.Divergence = s.StreamDivergences()
	d.Sources = s.transcriptSources()
	if dr := s.SourceDrift(); dr.Deleted+dr.Truncated+dr.Rewritten > 0 {
		d.SourceDrift = &dr
	}
	if strings.TrimSpace(repoFilter) != "" {
		d.Repo = s.repoDiagnostic(repoFilter)
	}
	return d
}

// newestIndexedTimestamp returns the freshest indexed entry timestamp
// (RFC3339), or "" when the index is empty.
func (s *Store) newestIndexedTimestamp() string {
	var ts sql.NullString
	_ = s.readDB.QueryRow(`SELECT MAX(timestamp) FROM entries WHERE timestamp IS NOT NULL`).Scan(&ts)
	if ts.Valid {
		return ts.String
	}
	return ""
}

// transcriptSources walks every configured project dir and reports its
// on-disk vs indexed coverage. The ingest cursors (offset + recorded
// size/mtime fingerprint, 🎯T68.6) classify each file's state.
func (s *Store) transcriptSources() []TranscriptSource {
	cursors := s.readIngestCursors()
	dirs := s.projectDirs()
	out := make([]TranscriptSource, 0, len(dirs))
	for _, dir := range dirs {
		ts := TranscriptSource{Path: dir}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			out = append(out, ts) // Exists=false
			continue
		}
		ts.Exists = true

		var newestMtime time.Time
		walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, werr error) error {
			if werr != nil || fi.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			ts.TotalFiles++
			mt := fi.ModTime().UTC()
			if mt.After(newestMtime) {
				newestMtime = mt
			}
			cur, known := cursors[path]
			size := fi.Size()

			var pending int64
			state := "converged"
			switch {
			case !known:
				state = "new"
				pending = size
				ts.UnknownOffset++
			case size > cur.offset:
				state = "append_behind"
				pending = size - cur.offset
			case size < cur.offset:
				state = "truncated"
			default:
				if cur.recordedSize.Valid && cur.recordedMtime.Valid &&
					size == cur.recordedSize.Int64 &&
					mt.Format(time.RFC3339Nano) > cur.recordedMtime.String {
					state = "rewritten"
				}
			}

			if pending > 0 {
				ts.BehindFiles++
				ts.PendingBytes += pending
				bf := BehindFile{
					Path:         path,
					ProjectDir:   filepath.Base(filepath.Dir(path)),
					SessionID:    strings.TrimSuffix(filepath.Base(path), ".jsonl"),
					Size:         size,
					Offset:       cur.offset,
					PendingBytes: pending,
					Mtime:        mt.Format(time.RFC3339),
					State:        state,
				}
				if cur.recordedMtime.Valid {
					bf.RecordedMtime = cur.recordedMtime.String
				}
				ts.Examples = insertTopBehind(ts.Examples, bf, maxBehindExamples)
			}
			return nil
		})
		ts.Readable = walkErr == nil
		if !newestMtime.IsZero() {
			ts.NewestFileMtime = newestMtime.Format(time.RFC3339)
		}
		out = append(out, ts)
	}
	return out
}

// insertTopBehind keeps the `max` largest-pending behind files. Bounds
// memory to `max` per source regardless of how many files are behind.
func insertTopBehind(list []BehindFile, bf BehindFile, max int) []BehindFile {
	list = append(list, bf)
	sort.SliceStable(list, func(i, j int) bool { return list[i].PendingBytes > list[j].PendingBytes })
	if len(list) > max {
		list = list[:max]
	}
	return list
}

// repoDiagnostic explains one repo's ingest state. It maps the repo to
// Claude project dirs by encoding the cwds of its indexed sessions
// (cwd→dir is deterministic; the reverse is ambiguous because the
// encoding collapses '/' and '.' to '-'), then compares the latest
// indexed activity against the newest on-disk transcript mtime.
func (s *Store) repoDiagnostic(repoFilter string) *RepoDiagnostic {
	rd := &RepoDiagnostic{RepoFilter: repoFilter}
	pattern := "%" + repoFilter + "%"

	cwds := map[string]bool{}
	if rows, err := s.readDB.Query(
		`SELECT DISTINCT cwd FROM session_meta WHERE cwd != '' AND (cwd LIKE ? OR repo LIKE ?)`,
		pattern, pattern); err == nil {
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil && c != "" {
				cwds[c] = true
			}
		}
		rows.Close()
	}

	var latest sql.NullString
	_ = s.readDB.QueryRow(`
		SELECT MAX(ss.last_msg)
		FROM session_summary ss
		JOIN session_meta sm ON sm.session_id = ss.session_id
		WHERE sm.cwd LIKE ? OR sm.repo LIKE ?`, pattern, pattern).Scan(&latest)
	if latest.Valid {
		rd.LatestIndexed = latest.String
	}

	dirs := s.projectDirs()
	var newestMtime time.Time
	seen := map[string]bool{}
	for cwd := range cwds {
		enc := encodeClaudeProjectDir(cwd)
		for _, pd := range dirs {
			cand := filepath.Join(pd, enc)
			if seen[cand] {
				continue
			}
			info, err := os.Stat(cand)
			if err != nil || !info.IsDir() {
				continue
			}
			seen[cand] = true
			rd.MatchedProjectDirs = append(rd.MatchedProjectDirs, cand)
			_ = filepath.Walk(cand, func(p string, fi os.FileInfo, e error) error {
				if e == nil && !fi.IsDir() && strings.HasSuffix(p, ".jsonl") {
					if mt := fi.ModTime().UTC(); mt.After(newestMtime) {
						newestMtime = mt
					}
				}
				return nil
			})
		}
	}
	sort.Strings(rd.MatchedProjectDirs)
	if !newestMtime.IsZero() {
		rd.LatestOnDiskMtime = newestMtime.Format(time.RFC3339)
	}

	switch {
	case len(rd.MatchedProjectDirs) == 0 && rd.LatestIndexed == "":
		rd.Note = "no transcript source maps to repo filter " + repoFilter +
			" — no indexed sessions and no matching ~/.claude/projects dir. " +
			"The repo may never have been indexed, or its sessions ran under a different cwd."
	case len(rd.MatchedProjectDirs) == 0:
		rd.Note = "indexed sessions exist for this repo but no matching project dir was found on disk " +
			"(transcripts pruned, or under a project dir mnemo isn't configured to watch)."
	case rd.LatestIndexed != "" && rd.LatestOnDiskMtime != "" && rd.LatestOnDiskMtime > rd.LatestIndexed:
		rd.Note = "on-disk transcripts are NEWER than the index for this repo — content is append-behind " +
			"(ingest lag) or the project dir isn't being watched."
	default:
		rd.Note = "index is current with on-disk transcripts for this repo."
	}
	return rd
}

// encodeClaudeProjectDir maps a working directory to the basename Claude
// Code uses for its transcript folder under ~/.claude/projects/: every
// '/' and '.' becomes '-' (e.g. /Users/m/work/github.com/squz/multimaze2
// → -Users-m-work-github-com-squz-multimaze2). The mapping is one-way:
// the reverse is ambiguous because literal '-' is indistinguishable from
// an encoded '/' or '.'.
func encodeClaudeProjectDir(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
}
