// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package threads is the shared core of the Threads feature (🎯T85): a
// menu-bar navigator that keeps one Claude Code session per initiative.
//
// A "thread" is a directory under a configurable root (default
// ~/think/threads/) holding a CLAUDE.md context file plus whatever working
// files accumulate. This package owns the filesystem model — listing,
// status/focus parsing, activity timestamps, scaffolding, archiving — and
// is deliberately free of any mnemo store/MCP/HTTP dependency so it unit-
// tests fast. The CLI, the mnemo_thread_* MCP tools, and the /api/thread/*
// endpoints are all thin adapters over a *Manager.
//
// The model is a live filesystem projection: nothing here is persisted to
// SQLite (mnemo's schema is append-only and threads add no tables).
package threads

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/todo"
)

// ContextFile is the per-thread context file name. It is excluded from the
// activity walk and the file count, and is what `show` renders.
const ContextFile = "CLAUDE.md"

// Reserved directory names under the root. They are never listed as threads
// and may not be created or archived.
const (
	TemplateDir = "_template"
	ArchivedDir = "_archived"
)

// NamePlaceholder is substituted in the template's CLAUDE.md on `new`.
const NamePlaceholder = "{{NAME}}"

// MarkerFile is the hidden file in a thread dir that stores the thread's
// marker (see Marker). Absent or empty means the default (normal) marker. It
// is a hidden file, so it is already excluded from the activity walk and file
// count.
const MarkerFile = ".marker"

// Marker is a per-thread tag shown in the leftmost column of the list. It is
// an open enum so more tags can be added later without a model change; today
// there are two — the default and "important", which pins the thread to the
// top of the list even when stale.
type Marker string

const (
	// MarkerNormal is the default marker (🪡). Persisted as an absent/empty
	// MarkerFile.
	MarkerNormal Marker = ""
	// MarkerImportant (❗️) pins the thread to the top of the list.
	MarkerImportant Marker = "important"
)

// MarkerInfo describes one marker for clients that build a marker menu without
// hardcoding the vocabulary. Every UI renders its menu from this catalog, so a
// new marker is one edit here — not a change in each head. Value is the
// persisted string, Label the human name, Pinned mirrors the sort behaviour.
type MarkerInfo struct {
	Value  string `json:"value"`
	Emoji  string `json:"emoji"`
	Label  string `json:"label"`
	Pinned bool   `json:"pinned"`
}

// markerCatalog is the single source of truth for the marker vocabulary, in
// display order. Emoji/Pinned/ParseMarker, set-marker validation, and the
// /api/thread/markers endpoint all derive from it, so adding a marker (e.g. a
// third tag) propagates to sorting and to every UI's menu with no further
// edits. The first entry is the default (normal) marker.
var markerCatalog = []MarkerInfo{
	{Value: string(MarkerNormal), Emoji: "🪡", Label: "Normal", Pinned: false},
	{Value: string(MarkerImportant), Emoji: "❗️", Label: "Important", Pinned: true},
}

// MarkerCatalog returns the ordered marker catalog. The slice is treated as
// immutable, so it is returned directly.
func MarkerCatalog() []MarkerInfo { return markerCatalog }

// markerInfoFor looks a marker up in the catalog.
func markerInfoFor(m Marker) (MarkerInfo, bool) {
	for _, mi := range markerCatalog {
		if mi.Value == string(m) {
			return mi, true
		}
	}
	return MarkerInfo{}, false
}

// Emoji returns the marker's glyph, falling back to the default (normal)
// marker's glyph for unknown stored values, so an older binary reading a
// future marker degrades gracefully.
func (m Marker) Emoji() string {
	if mi, ok := markerInfoFor(m); ok {
		return mi.Emoji
	}
	return markerCatalog[0].Emoji
}

// Pinned reports whether the marker pins its thread to the top of the list.
func (m Marker) Pinned() bool {
	mi, _ := markerInfoFor(m)
	return mi.Pinned
}

// ParseMarker maps a stored string to a known Marker, or MarkerNormal when
// unrecognised.
func ParseMarker(s string) Marker {
	m := Marker(strings.TrimSpace(s))
	if _, ok := markerInfoFor(m); ok {
		return m
	}
	return MarkerNormal
}

// nameRE is the kebab-case constraint on thread names: lowercase
// alphanumeric and hyphens, not starting with a hyphen.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Manager exposes thread operations rooted at a single threads directory.
// Home is the user's home directory, used both to locate each thread's
// Claude Code transcript directory and to render ~-relative paths.
type Manager struct {
	// Root is the absolute, resolved threads root (e.g. ~/think/threads).
	Root string
	// Home is the absolute user home directory.
	Home string
}

// Thread is the projected view of one thread directory.
type Thread struct {
	Name string `json:"name"`
	// Path is the absolute directory path.
	Path string `json:"path"`
	// Status is the first meaningful line of the ## Status section ("" if none).
	Status string `json:"status"`
	// State is the first word of Status, lowercased and stripped ("" if none).
	State string `json:"state"`
	// Focus is the first meaningful line of the ## Current focus section.
	Focus string `json:"focus"`
	// FileCount is the number of top-level, non-hidden, non-CLAUDE.md regular files.
	FileCount int `json:"file_count"`
	// Activity is the most recent activity time; zero when HasActivity is false.
	Activity time.Time `json:"activity"`
	// HasActivity reports whether any activity timestamp was found.
	HasActivity bool `json:"has_activity"`
	// Marker is the thread's tag (default or important). Persisted in the
	// MarkerFile. Pinned markers sort the thread above everything else, even
	// when stale.
	Marker Marker `json:"marker"`
	// ActiveTodos / OverdueTodos count the thread's open (not done/cancelled)
	// TODO items and those past their due date, parsed from todo.md/todos.md
	// at any depth.
	ActiveTodos  int `json:"active_todos"`
	OverdueTodos int `json:"overdue_todos"`
}

// IsReserved reports whether name is a reserved or hidden directory (not a
// thread): anything starting with "_" or "." — which covers _template and
// _archived.
func IsReserved(name string) bool {
	return name == "" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
}

// ValidateName checks name against the kebab-case rule and rejects reserved
// names. It does not check for existence.
func ValidateName(name string) error {
	if IsReserved(name) {
		return fmt.Errorf("name %q is reserved (must not start with _ or .)", name)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("name %q must be kebab-case: %s", name, nameRE.String())
	}
	return nil
}

// List returns all threads under the root, unsorted. A missing root is not
// an error — it yields an empty list (the root is created lazily on `new`).
func (m *Manager) List() ([]Thread, error) {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read threads root %s: %w", m.Root, err)
	}
	var out []Thread
	for _, e := range entries {
		if !e.IsDir() || IsReserved(e.Name()) {
			continue
		}
		t, err := m.load(e.Name())
		if err != nil {
			// A single unreadable thread should not sink the whole list.
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// ListSorted returns threads ordered the way the UI and CLI present them:
// threads with activity first (newest first), then activity-less threads,
// ties broken by name.
func (m *Manager) ListSorted() ([]Thread, error) {
	ts, err := m.List()
	if err != nil {
		return nil, err
	}
	SortByActivity(ts)
	return ts, nil
}

// SortByActivity orders threads in place: pinned (important) threads first
// regardless of activity, then active before inactive, newer before older,
// name as the tiebreak.
func SortByActivity(ts []Thread) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Marker.Pinned() != b.Marker.Pinned() {
			return a.Marker.Pinned() // pinned sorts to the top, even when stale
		}
		if a.HasActivity != b.HasActivity {
			return a.HasActivity // active sorts before inactive
		}
		if a.HasActivity && !a.Activity.Equal(b.Activity) {
			return a.Activity.After(b.Activity)
		}
		return a.Name < b.Name
	})
}

// Get returns a single thread by name. Unknown or reserved names return an
// error.
func (m *Manager) Get(name string) (Thread, error) {
	if IsReserved(name) {
		return Thread{}, fmt.Errorf("name %q is reserved", name)
	}
	fi, err := os.Stat(filepath.Join(m.Root, name))
	if err != nil || !fi.IsDir() {
		return Thread{}, fmt.Errorf("thread %q not found", name)
	}
	return m.load(name)
}

// load builds the Thread projection for an existing directory named name.
func (m *Manager) load(name string) (Thread, error) {
	dir := filepath.Join(m.Root, name)
	t := Thread{
		Name:   name,
		Path:   dir,
		Marker: m.markerOf(name),
	}

	if data, err := os.ReadFile(filepath.Join(dir, ContextFile)); err == nil {
		t.Status = extractSection(string(data), "Status")
		t.State = firstWordState(t.Status)
		t.Focus = extractSection(string(data), "Current focus")
	}

	t.FileCount = m.fileCount(dir)
	if act, ok := m.activity(dir); ok {
		t.Activity, t.HasActivity = act, true
	}
	t.ActiveTodos, t.OverdueTodos = m.todoCounts(dir)
	return t, nil
}

// todoCounts walks the thread dir for todo.md/todos.md files (at any depth,
// matching mnemo's todo discovery) and counts open items (active) and those
// past their due date (overdue), using mnemo's todo parser so the numbers
// match what mnemo reports. Hidden directories are skipped.
func (m *Manager) todoCounts(dir string) (active, overdue int) {
	today := time.Now().Format("2006-01-02")
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isTodoFileName(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, task := range todo.Parse(string(data)) {
			if task.Status == todo.StatusDone || task.Status == todo.StatusCancelled {
				continue
			}
			active++
			if task.Due != "" && task.Due < today {
				overdue++
			}
		}
		return nil
	})
	return active, overdue
}

// isTodoFileName reports whether base is a default TODO file name, matching
// mnemo's store-side discovery.
func isTodoFileName(base string) bool {
	switch strings.ToLower(base) {
	case "todo.md", "todos.md":
		return true
	}
	return false
}

// fileCount counts top-level regular files in dir, excluding hidden files
// and CLAUDE.md.
func (m *Manager) fileCount(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == ContextFile {
			continue
		}
		if e.Type().IsRegular() {
			n++
		}
	}
	return n
}

// activity returns the thread's activity timestamp: the max of the newest
// regular-file mtime anywhere under the thread dir (excluding hidden files
// and CLAUDE.md) and the newest mtime in the thread's Claude Code transcript
// directory. The bool is false when neither walk found anything.
func (m *Manager) activity(dir string) (time.Time, bool) {
	var newest time.Time
	found := false
	note := func(t time.Time) {
		if t.After(newest) {
			newest, found = t, true
		}
	}

	// 1. Working files under the thread dir (recursive), excluding hidden
	//    paths and CLAUDE.md.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		base := d.Name()
		if path != dir && strings.HasPrefix(base, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || base == ContextFile || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			note(fi.ModTime())
		}
		return nil
	})

	// 2. The thread's transcript directory. Conversing with a thread you are
	//    not editing files in still counts as activity.
	if td := m.transcriptDir(dir); td != "" {
		if entries, err := os.ReadDir(td); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
					continue
				}
				if fi, err := e.Info(); err == nil {
					note(fi.ModTime())
				}
			}
		}
	}

	return newest, found
}

// transcriptDir returns the Claude Code transcript directory for a thread
// path: <home>/.claude/projects/<encoded>, where <encoded> flattens the
// absolute path into a single directory-name component. On macOS that is
// just "/"→"-"; on Windows the backslash separators and the drive colon
// are flattened too, so the result is always a valid single path
// component rather than a path that re-introduces separators. (Exact
// correlation with Claude Code's Windows transcript-dir naming is a 🎯T89
// concern; here it only needs to be a valid path so activity detection
// degrades to "none" off macOS rather than erroring.) Returns "" when
// Home is unset.
func (m *Manager) transcriptDir(absThreadPath string) string {
	if m.Home == "" {
		return ""
	}
	encoded := encodeTranscriptDir(absThreadPath)
	return filepath.Join(m.Home, ".claude", "projects", encoded)
}

// encodeTranscriptDir flattens an absolute path into a single Claude Code
// projects/ directory-name component. Separators ("/" and, on Windows,
// "\") and the Windows drive colon all map to "-".
func encodeTranscriptDir(absPath string) string {
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(absPath)
}

// FileEntry is one entry in a thread's working-file listing (used by
// `show`). Directories are included with IsDir set.
type FileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// Files lists a thread's top-level entries excluding hidden files and
// CLAUDE.md, newest first.
func (m *Manager) Files(name string) ([]FileEntry, error) {
	if IsReserved(name) {
		return nil, fmt.Errorf("name %q is reserved", name)
	}
	entries, err := os.ReadDir(filepath.Join(m.Root, name))
	if err != nil {
		return nil, fmt.Errorf("read thread %q: %w", name, err)
	}
	var out []FileEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || e.Name() == ContextFile {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		fe := FileEntry{Name: e.Name(), IsDir: e.IsDir(), ModTime: fi.ModTime()}
		if !e.IsDir() {
			fe.Size = fi.Size()
		}
		out = append(out, fe)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out, nil
}

// Search returns the names of threads whose content matches q (the deep
// channel for the menu-bar shim's search, §6): a case-insensitive match on the
// thread name, its CLAUDE.md body, or any of its working-file names. This is a
// live grep over the (small) set of threads — no index — so it has no schema
// or FTS dependency. An empty query matches nothing.
func (m *Manager) Search(q string) []string {
	needle := strings.ToLower(strings.TrimSpace(q))
	if needle == "" {
		return nil
	}
	ts, err := m.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range ts {
		if threadMatches(t.Name, needle) || m.contentMatches(t.Name, needle) {
			out = append(out, t.Name)
		}
	}
	return out
}

func threadMatches(name, needle string) bool {
	return strings.Contains(strings.ToLower(name), needle)
}

func (m *Manager) contentMatches(name, needle string) bool {
	if body, err := m.ReadContext(name); err == nil {
		if strings.Contains(strings.ToLower(body), needle) {
			return true
		}
	}
	files, err := m.Files(name)
	if err != nil {
		return false
	}
	for _, f := range files {
		if strings.Contains(strings.ToLower(f.Name), needle) {
			return true
		}
	}
	return false
}

// ReadContext returns the raw CLAUDE.md body for a thread, or an error if the
// thread or its context file is missing.
func (m *Manager) ReadContext(name string) (string, error) {
	if IsReserved(name) {
		return "", fmt.Errorf("name %q is reserved", name)
	}
	data, err := os.ReadFile(filepath.Join(m.Root, name, ContextFile))
	if err != nil {
		return "", fmt.Errorf("read %s for thread %q: %w", ContextFile, name, err)
	}
	return string(data), nil
}

// markerOf reads a thread's marker from its MarkerFile, defaulting to
// MarkerNormal when absent or unrecognised.
func (m *Manager) markerOf(name string) Marker {
	data, err := os.ReadFile(filepath.Join(m.Root, name, MarkerFile))
	if err != nil {
		return MarkerNormal
	}
	return ParseMarker(string(data))
}

// SetMarker sets a thread's marker, persisting it in the MarkerFile.
// MarkerNormal removes the file. Reserved names are refused.
func (m *Manager) SetMarker(name string, mk Marker) error {
	if IsReserved(name) {
		return fmt.Errorf("name %q is reserved", name)
	}
	if fi, err := os.Stat(filepath.Join(m.Root, name)); err != nil || !fi.IsDir() {
		return fmt.Errorf("thread %q not found", name)
	}
	path := filepath.Join(m.Root, name, MarkerFile)
	if mk == MarkerNormal {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear marker: %w", err)
		}
		return nil
	}
	if _, ok := markerInfoFor(mk); !ok {
		return fmt.Errorf("unknown marker %q", mk)
	}
	if err := os.WriteFile(path, []byte(string(mk)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
}

// NewArgs parameterises thread creation.
type NewArgs struct {
	// Name is the kebab-case thread name.
	Name string
}

// New scaffolds a thread: validate the name, reject reserved/existing names,
// copy _template/CLAUDE.md substituting {{NAME}}, and write the new
// CLAUDE.md. Nothing else is created. The threads root and a fallback
// template are created on demand so a first-run user is not blocked.
func (m *Manager) New(args NewArgs) (Thread, error) {
	if err := ValidateName(args.Name); err != nil {
		return Thread{}, err
	}
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return Thread{}, fmt.Errorf("create threads root: %w", err)
	}
	dir := filepath.Join(m.Root, args.Name)
	if _, err := os.Stat(dir); err == nil {
		return Thread{}, fmt.Errorf("thread %q already exists", args.Name)
	}

	tmpl, err := m.templateBody()
	if err != nil {
		return Thread{}, err
	}
	body := strings.ReplaceAll(tmpl, NamePlaceholder, args.Name)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Thread{}, fmt.Errorf("create thread dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ContextFile), []byte(body), 0o644); err != nil {
		return Thread{}, fmt.Errorf("write %s: %w", ContextFile, err)
	}
	return m.load(args.Name)
}

// templateBody returns the contents of _template/CLAUDE.md. Scaffolding always
// uses the on-disk template; the built-in default is only ever used to SEED it
// when it is missing (so the template is explicit and user-editable, and a
// thread created today matches one created tomorrow regardless of binary
// version). See EnsureTemplate.
func (m *Manager) templateBody() (string, error) {
	if err := m.EnsureTemplate(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(m.Root, TemplateDir, ContextFile))
	if err != nil {
		return "", fmt.Errorf("read template: %w", err)
	}
	return string(data), nil
}

// EnsureTemplate seeds _template/CLAUDE.md from the built-in default when it
// does not already exist. Idempotent: an existing template (incl. one the user
// has edited) is left untouched.
func (m *Manager) EnsureTemplate() error {
	path := filepath.Join(m.Root, TemplateDir, ContextFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat template: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(m.Root, TemplateDir), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", TemplateDir, err)
	}
	if err := os.WriteFile(path, []byte(defaultTemplate), 0o644); err != nil {
		return fmt.Errorf("seed template: %w", err)
	}
	return nil
}

// defaultTemplate is the fallback scaffold used when _template/CLAUDE.md is
// absent. The boilerplate H1 + preamble sit inside a preview-skip region so
// they stay in the file for Claude's context but are omitted from the hover
// preview (which prepends its own synthetic H1 of the dir name, §5). The
// italic placeholder lines read as empty under the section extractor until
// filled in.
const defaultTemplate = `<!-- preview-skip -->
# {{NAME}}

This is a Claude Code thread. Fill in the sections below.
<!-- /preview-skip -->

## Status

_active_

## Current focus

_what this thread is about_
`

// Archive moves <root>/<name>/ to <root>/_archived/<name>/. Reserved names
// are refused; an existing destination is an error; _archived/ is created on
// demand. No compression.
func (m *Manager) Archive(name string) error {
	if IsReserved(name) {
		return fmt.Errorf("name %q is reserved and cannot be archived", name)
	}
	src := filepath.Join(m.Root, name)
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return fmt.Errorf("thread %q not found", name)
	}
	archRoot := filepath.Join(m.Root, ArchivedDir)
	if err := os.MkdirAll(archRoot, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", ArchivedDir, err)
	}
	dst := filepath.Join(archRoot, name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("archive destination %q already exists", dst)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("archive %q: %w", name, err)
	}
	return nil
}

// Resolve maps a `go` argument — a bare kebab name (resolved under the root)
// or an absolute/~ path — to an absolute thread directory path. It does not
// require the directory to exist (the caller decides whether to spawn).
func (m *Manager) Resolve(nameOrPath string) (string, error) {
	s := strings.TrimSpace(nameOrPath)
	if s == "" {
		return "", fmt.Errorf("empty thread reference")
	}
	switch {
	case s == "~":
		return m.Home, nil
	case strings.HasPrefix(s, "~/"):
		return filepath.Join(m.Home, s[2:]), nil
	case filepath.IsAbs(s):
		return filepath.Clean(s), nil
	}
	// Bare reference: a kebab name resolved under the root. Reject path
	// separators so "../etc" can't escape the root.
	if strings.ContainsRune(s, filepath.Separator) {
		return "", fmt.Errorf("bare thread reference %q must not contain a path separator", s)
	}
	if err := ValidateName(s); err != nil {
		return "", err
	}
	return filepath.Join(m.Root, s), nil
}
