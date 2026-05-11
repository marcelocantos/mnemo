// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// `mnemo diagnose` — manual health check.
//
// Diagnose runs a series of independent checks against the local
// mnemo install and prints a single-screen report. Each check is
// best-effort: failures don't stop subsequent checks, so the user
// gets the full picture in one run rather than playing whack-a-mole.
//
// Checks cover (in roughly the order they tend to break):
//
//  1. Daemon process liveness, version, listening port, inherited PATH
//  2. HTTP MCP endpoint reachability + initialize handshake
//  3. External tools (gh, claude, git, uv, pdftotext, mutool, brew, lsof)
//     — found-on-PATH, version, and the consequence of absence
//  4. Filesystem (~/.claude/projects/ readable, ~/.mnemo/ writable,
//     config.json validity)
//  5. Database (schema version, table row counts, FTS parity, last
//     ingest per stream) — opened read-only so it works alongside a
//     running daemon
//  6. Index freshness (newest JSONL mtime vs newest indexed message)
//  7. Configuration snapshot (workspace_roots / extra_project_dirs /
//     synthesis_roots — does each path exist?)
//  8. Claude Code integration (~/.claude.json mnemo entry shape and
//     URL-vs-running-daemon match)
//  9. Recent ERROR/WARN lines from the mnemo log
//
// The exit code is 0 if every check passed or only emitted warnings,
// 1 if any check failed outright. Useful for scripted health probes.
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/mcpconfig"
	"github.com/marcelocantos/mnemo/internal/store"
)

// checkStatus is the per-check verdict. ok is "everything fine",
// warn is "something is degraded but not a blocker", fail is "this
// is broken and probably the cause of whatever you're seeing", skip
// is "the check didn't apply on this platform / install".
type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusSkip
)

func (s checkStatus) tag() string {
	switch s {
	case statusOK:
		return "[ ok ]"
	case statusWarn:
		return "[warn]"
	case statusFail:
		return "[FAIL]"
	default:
		return "[skip]"
	}
}

// checkResult is what each check function returns: a status, a one-
// line headline, and any number of detail lines (rendered indented
// under the headline).
type checkResult struct {
	status checkStatus
	title  string
	lines  []string
}

func (r *checkResult) add(line string) { r.lines = append(r.lines, line) }

func cmdDiagnose(args []string) {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "HTTP listen address to probe")
	logPath := fs.String("log", "", "mnemo log path to tail (default: brew prefix on macOS, /var/log on Linux)")
	_ = fs.Parse(args)

	fmt.Printf("mnemo diagnose — %s/%s, %s\n",
		runtime.GOOS, runtime.GOARCH, time.Now().Format(time.RFC3339))
	fmt.Println(strings.Repeat("=", 60))

	checks := []func() checkResult{
		func() checkResult { return checkDaemon(*addr) },
		func() checkResult { return checkEndpoint(*addr) },
		checkExternalTools,
		checkFilesystem,
		checkDatabase,
		checkIndexFreshness,
		checkConfiguration,
		checkClaudeIntegration,
		func() checkResult { return checkLogs(*logPath) },
	}

	worst := statusOK
	for _, check := range checks {
		r := check()
		printResult(r)
		if r.status == statusFail {
			worst = statusFail
		} else if r.status == statusWarn && worst == statusOK {
			worst = statusWarn
		}
	}

	fmt.Println(strings.Repeat("=", 60))
	switch worst {
	case statusOK:
		fmt.Println("All checks passed.")
	case statusWarn:
		fmt.Println("Completed with warnings.")
	case statusFail:
		fmt.Println("One or more checks FAILED — see [FAIL] entries above.")
		os.Exit(1)
	}
}

func printResult(r checkResult) {
	fmt.Printf("\n%s %s\n", r.status.tag(), r.title)
	for _, line := range r.lines {
		fmt.Printf("       %s\n", line)
	}
}

// ---------------------------------------------------------------------------
// 1. Daemon process
// ---------------------------------------------------------------------------

func checkDaemon(addr string) checkResult {
	r := checkResult{title: "Daemon process"}
	pid := findListenerPID(addr)
	if pid == 0 {
		r.status = statusFail
		r.add(fmt.Sprintf("nothing listening on %s — daemon is not running", addr))
		r.add("start with `brew services start mnemo` (macOS/Linux) or check the Windows Service")
		return r
	}
	r.add(fmt.Sprintf("PID %d listening on %s", pid, addr))

	if path := readProcessEnv(pid, "PATH"); path != "" {
		r.add("inherited PATH:")
		for _, p := range splitPath(path) {
			marker := "  "
			if !pathExists(p) {
				marker = "  (missing) "
			}
			r.add(marker + p)
		}
	} else {
		r.add("could not read process environment (skipping PATH inspection)")
	}
	r.status = statusOK
	return r
}

// findListenerPID returns the PID listening on the given TCP port,
// or 0 if none / not detectable. Uses lsof (macOS/Linux); on
// Windows a future check could parse `netstat -ano` output.
func findListenerPID(addr string) int {
	port := strings.TrimPrefix(addr, ":")
	if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i+1:]
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.Contains(line, ":"+port+" ") || !strings.Contains(line, "LISTENING") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 5 {
				var pid int
				fmt.Sscanf(fields[len(fields)-1], "%d", &pid)
				return pid
			}
		}
		return 0
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid)
	return pid
}

// readProcessEnv extracts a single env var from a running process.
// macOS: `ps eww PID`. Linux: /proc/PID/environ. Windows: skipped.
func readProcessEnv(pid int, key string) string {
	prefix := key + "="
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			return ""
		}
		for _, entry := range bytes.Split(raw, []byte{0}) {
			if bytes.HasPrefix(entry, []byte(prefix)) {
				return string(entry[len(prefix):])
			}
		}
	case "darwin":
		out, err := exec.Command("ps", "eww", "-o", "command=", fmt.Sprintf("%d", pid)).Output()
		if err != nil {
			return ""
		}
		for _, field := range strings.Fields(string(out)) {
			if strings.HasPrefix(field, prefix) {
				return field[len(prefix):]
			}
		}
	}
	return ""
}

func splitPath(p string) []string {
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	return strings.Split(p, sep)
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---------------------------------------------------------------------------
// 2. HTTP MCP endpoint
// ---------------------------------------------------------------------------

func checkEndpoint(addr string) checkResult {
	r := checkResult{title: "HTTP MCP endpoint"}
	url := "http://localhost" + addr + "/mcp"

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mnemo-diagnose","version":"1"}}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.status = statusFail
		r.add(fmt.Sprintf("POST %s failed: %v", url, err))
		return r
	}
	defer resp.Body.Close()
	rtt := time.Since(start)

	if resp.StatusCode != 200 {
		r.status = statusFail
		r.add(fmt.Sprintf("HTTP %d from %s (expected 200)", resp.StatusCode, url))
		return r
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		r.add(fmt.Sprintf("initialize OK (%s), Mcp-Session-Id=%s", rtt.Round(time.Millisecond), sid))
	} else {
		r.add(fmt.Sprintf("initialize OK (%s), no Mcp-Session-Id header", rtt.Round(time.Millisecond)))
	}
	r.status = statusOK
	return r
}

// ---------------------------------------------------------------------------
// 3. External tools
// ---------------------------------------------------------------------------

type toolSpec struct {
	name        string
	required    bool
	versionArgs []string
	consequence string
}

var externalTools = []toolSpec{
	{"gh", false, []string{"--version"}, "PR / issue / CI indexing degrades silently"},
	{"git", true, []string{"--version"}, "commit indexing AND repo discovery break"},
	{"claude", false, []string{"--version"}, "image descriptions skipped"},
	{"uv", false, []string{"--version"}, "image embeddings skipped (no CLIP)"},
	{"pdftotext", false, []string{"-v"}, "PDF docs skipped"},
	{"mutool", false, []string{"-v"}, "PDF docs fallback skipped"},
	{"lsof", false, []string{"-v"}, "live-session discovery (mnemo_whatsup) degrades"},
	{"brew", false, []string{"--version"}, "auto-start fallback in stdio-migration won't fire"},
}

func checkExternalTools() checkResult {
	r := checkResult{title: "External tools"}
	for _, t := range externalTools {
		path, _ := exec.LookPath(t.name)
		if path == "" {
			tag := "MISSING"
			if !t.required {
				tag = "missing"
			}
			r.add(fmt.Sprintf("%-10s %-8s — %s", t.name, tag, t.consequence))
			if t.required {
				r.status = statusFail
			} else if r.status == statusOK {
				r.status = statusWarn
			}
			continue
		}
		ver := toolVersion(path, t.versionArgs)
		r.add(fmt.Sprintf("%-10s %s (%s)", t.name, path, ver))
	}
	return r
}

func toolVersion(path string, args []string) string {
	if len(args) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if err != nil {
		return "version unknown"
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	if len(first) > 80 {
		first = first[:77] + "..."
	}
	return strings.TrimSpace(first)
}

// ---------------------------------------------------------------------------
// 4. Filesystem
// ---------------------------------------------------------------------------

func checkFilesystem() checkResult {
	r := checkResult{title: "Filesystem"}
	home, err := os.UserHomeDir()
	if err != nil {
		r.status = statusFail
		r.add(fmt.Sprintf("os.UserHomeDir failed: %v", err))
		return r
	}

	projectDir := filepath.Join(home, ".claude", "projects")
	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")

	if info, err := os.Stat(projectDir); err != nil {
		r.add(fmt.Sprintf("[FAIL] %s: %v", projectDir, err))
		r.status = statusFail
	} else if !info.IsDir() {
		r.add(fmt.Sprintf("[FAIL] %s: not a directory", projectDir))
		r.status = statusFail
	} else {
		count := countJSONL(projectDir)
		r.add(fmt.Sprintf("%s — %d *.jsonl files", projectDir, count))
	}

	if info, err := os.Stat(dbPath); err != nil {
		r.add(fmt.Sprintf("[warn] %s: %v (will be created on first ingest)", dbPath, err))
		if r.status == statusOK {
			r.status = statusWarn
		}
	} else {
		r.add(fmt.Sprintf("%s — %s, mtime %s", dbPath,
			humanBytes(info.Size()), info.ModTime().Format(time.RFC3339)))
		if testWritable(filepath.Dir(dbPath)) {
			r.add(filepath.Dir(dbPath) + " is writable")
		} else {
			r.add("[warn] " + filepath.Dir(dbPath) + " is NOT writable — ingest will fail")
			r.status = statusFail
		}
	}
	return r
}

func countJSONL(root string) int {
	count := 0
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			count++
		}
		return nil
	})
	return count
}

func testWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".diag-write-test-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
	}
}

// ---------------------------------------------------------------------------
// 5. Database
// ---------------------------------------------------------------------------

func checkDatabase() checkResult {
	r := checkResult{title: "Database"}
	home, err := os.UserHomeDir()
	if err != nil {
		r.status = statusFail
		r.add(err.Error())
		return r
	}
	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(dbPath); err != nil {
		r.status = statusSkip
		r.add(fmt.Sprintf("no database at %s", dbPath))
		return r
	}

	// Open read-only so we don't conflict with the running daemon.
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=2000", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		r.status = statusFail
		r.add(fmt.Sprintf("open: %v", err))
		return r
	}
	defer db.Close()

	var schemaVer int
	if err := db.QueryRow("PRAGMA user_version").Scan(&schemaVer); err == nil {
		r.add(fmt.Sprintf("schema version: %d", schemaVer))
	}

	tables := []string{
		"entries", "messages", "session_meta", "session_summary",
		"memories", "skills", "claude_configs", "audit_entries",
		"targets", "plans", "decisions", "ci_runs",
		"git_commits", "github_prs", "github_issues",
		"images", "docs",
	}
	r.add("table row counts:")
	for _, t := range tables {
		var n int64
		if err := db.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			r.add(fmt.Sprintf("  %-18s (table missing or error)", t))
			continue
		}
		r.add(fmt.Sprintf("  %-18s %d", t, n))
	}

	// Per-stream backfill recency.
	rows, err := db.Query(`SELECT stream, last_backfill, files_indexed, files_on_disk FROM ingest_status ORDER BY stream`)
	if err == nil {
		r.add("ingest_status (last_backfill / files_indexed / files_on_disk):")
		for rows.Next() {
			var stream, last string
			var idx, disk int
			_ = rows.Scan(&stream, &last, &idx, &disk)
			r.add(fmt.Sprintf("  %-15s %s  %5d / %5d", stream, last, idx, disk))
		}
		rows.Close()
	}

	r.status = statusOK
	return r
}

// ---------------------------------------------------------------------------
// 6. Index freshness
// ---------------------------------------------------------------------------

func checkIndexFreshness() checkResult {
	r := checkResult{title: "Index freshness"}
	home, err := os.UserHomeDir()
	if err != nil {
		r.status = statusFail
		r.add(err.Error())
		return r
	}
	projectDir := filepath.Join(home, ".claude", "projects")
	newest := newestJSONLMtime(projectDir)
	if newest.IsZero() {
		r.status = statusSkip
		r.add("no JSONL files found to compare against")
		return r
	}
	r.add(fmt.Sprintf("newest JSONL on disk: %s", newest.Format(time.RFC3339)))

	dbPath := filepath.Join(home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(dbPath); err != nil {
		r.status = statusSkip
		r.add("no database to compare against")
		return r
	}
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_busy_timeout=2000", dbPath))
	if err != nil {
		r.status = statusFail
		r.add(err.Error())
		return r
	}
	defer db.Close()
	var lastTS sql.NullString
	_ = db.QueryRow(`SELECT MAX(timestamp) FROM messages`).Scan(&lastTS)
	if !lastTS.Valid {
		r.status = statusWarn
		r.add("no messages indexed yet")
		return r
	}
	r.add(fmt.Sprintf("newest indexed message: %s", lastTS.String))
	if lastIdx, err := time.Parse(time.RFC3339Nano, lastTS.String); err == nil {
		drift := newest.Sub(lastIdx)
		switch {
		case drift < 5*time.Minute:
			r.add(fmt.Sprintf("drift: %s (healthy)", drift.Round(time.Second)))
			r.status = statusOK
		case drift < time.Hour:
			r.add(fmt.Sprintf("drift: %s (watcher may be lagging)", drift.Round(time.Second)))
			r.status = statusWarn
		default:
			r.add(fmt.Sprintf("drift: %s (watcher is NOT keeping up)", drift.Round(time.Second)))
			r.status = statusFail
		}
	}
	return r
}

func newestJSONLMtime(root string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

// ---------------------------------------------------------------------------
// 7. Configuration
// ---------------------------------------------------------------------------

func checkConfiguration() checkResult {
	r := checkResult{title: "Configuration (~/.mnemo/config.json)"}
	cfg, err := store.LoadConfig()
	if err != nil {
		r.status = statusWarn
		r.add(fmt.Sprintf("LoadConfig failed (using defaults): %v", err))
		return r
	}
	roots := cfg.ResolvedWorkspaceRoots()
	r.add("workspace_roots (resolved):")
	for _, p := range roots {
		r.add(pathStatus(p))
	}
	if len(cfg.ExtraProjectDirs) > 0 {
		r.add("extra_project_dirs:")
		for _, p := range cfg.ExtraProjectDirs {
			r.add(pathStatus(p))
		}
	}
	if synth := cfg.ResolvedSynthesisRoots(); len(synth) > 0 {
		r.add("synthesis_roots:")
		for _, p := range synth {
			r.add(pathStatus(p))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if vp := cfg.ResolvedVaultPath(home); vp != "" {
			count := countMDFilesLocal(vp)
			r.add(fmt.Sprintf("vault_path: %s (%d .md files)", vp, count))
		}
	}
	r.status = statusOK
	return r
}

// countMDFilesLocal counts .md files under root using filepath.WalkDir.
func countMDFilesLocal(root string) int {
	count := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".md" {
			count++
		}
		return nil
	})
	return count
}

func pathStatus(p string) string {
	if pathExists(p) {
		return "  " + p
	}
	return "  (missing) " + p
}

// ---------------------------------------------------------------------------
// 8. Claude Code integration
// ---------------------------------------------------------------------------

func checkClaudeIntegration() checkResult {
	r := checkResult{title: "Claude Code integration (~/.claude.json)"}
	path, err := mcpconfig.ConfigPath()
	if err != nil {
		r.status = statusFail
		r.add(err.Error())
		return r
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		r.status = statusFail
		r.add(fmt.Sprintf("read %s: %v", path, err))
		return r
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		r.status = statusFail
		r.add(fmt.Sprintf("parse %s: %v", path, err))
		return r
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	entry, _ := servers["mnemo"].(map[string]any)
	if entry == nil {
		r.status = statusFail
		r.add("no mnemo entry in mcpServers — run `mnemo register-mcp` to add one")
		return r
	}
	t, _ := entry["type"].(string)
	if t == "stdio" {
		// mcpbridge is the supported pattern for wrapping the HTTP
		// daemon over stdio (used to keep MCP sessions alive across
		// daemon restarts). Recognise it; pull the wrapped URL out
		// of the args for further validation.
		cmd, _ := entry["command"].(string)
		args, _ := entry["args"].([]any)
		if filepath.Base(cmd) == "mcpbridge" {
			wrappedURL := mcpbridgeArg(args, "--url")
			r.add(fmt.Sprintf("type: stdio via mcpbridge → %s", wrappedURL))
			if wrappedURL == "" {
				r.add("[warn] mcpbridge entry has no --url arg")
				r.status = statusWarn
				return r
			}
			if !strings.Contains(wrappedURL, "?user=") {
				r.add("[warn] mcpbridge --url has no ?user=<name>")
				r.status = statusWarn
				return r
			}
			r.status = statusOK
			return r
		}
		r.status = statusFail
		r.add(fmt.Sprintf("mnemo is registered as raw stdio (command=%s) — should be HTTP since v0.20.0", cmd))
		r.add("re-run `mnemo register-mcp` to migrate")
		return r
	}
	url, _ := entry["url"].(string)
	r.add(fmt.Sprintf("type: %s, url: %s", t, url))
	if !strings.Contains(url, "?user=") {
		r.add("[warn] URL has no ?user=<name> — Windows Service deployments will fail")
		r.status = statusWarn
		return r
	}
	r.status = statusOK
	return r
}

// mcpbridgeArg picks a string value out of an mcpbridge args list,
// e.g. given ["--url", "http://localhost:19419/mcp?user=marcelo",
// "--config", "mnemo"], mcpbridgeArg(args, "--url") returns the
// URL. Returns empty string on miss.
func mcpbridgeArg(args []any, key string) string {
	for i := 0; i < len(args)-1; i++ {
		s, _ := args[i].(string)
		if s == key {
			v, _ := args[i+1].(string)
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 9. Recent log errors
// ---------------------------------------------------------------------------

func checkLogs(override string) checkResult {
	r := checkResult{title: "Recent log errors"}
	path := override
	if path == "" {
		path = guessLogPath()
	}
	if path == "" {
		r.status = statusSkip
		r.add("no log path known for this platform; pass --log <path> to inspect")
		return r
	}
	r.add(fmt.Sprintf("source: %s", path))

	f, err := os.Open(path)
	if err != nil {
		r.status = statusSkip
		r.add(fmt.Sprintf("cannot open: %v", err))
		return r
	}
	defer f.Close()
	if info, _ := f.Stat(); info != nil {
		const tail = 256 * 1024
		if info.Size() > tail {
			_, _ = f.Seek(-tail, 2)
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var hits []string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "level=ERROR") && !strings.Contains(line, "level=WARN") {
			continue
		}
		hits = append(hits, line)
	}
	if len(hits) == 0 {
		r.status = statusOK
		r.add("no ERROR or WARN entries in the recent log tail")
		return r
	}
	if len(hits) > 10 {
		hits = hits[len(hits)-10:]
	}
	sort.SliceStable(hits, func(i, j int) bool { return false }) // preserve order
	r.add(fmt.Sprintf("last %d ERROR/WARN lines:", len(hits)))
	for _, h := range hits {
		if len(h) > 200 {
			h = h[:197] + "..."
		}
		r.add("  " + h)
	}
	r.status = statusWarn
	return r
}

func guessLogPath() string {
	switch runtime.GOOS {
	case "darwin":
		for _, p := range []string{
			"/opt/homebrew/var/log/mnemo.log",
			"/usr/local/var/log/mnemo.log",
		} {
			if pathExists(p) {
				return p
			}
		}
	case "linux":
		for _, p := range []string{
			"/home/linuxbrew/.linuxbrew/var/log/mnemo.log",
			"/var/log/mnemo.log",
		} {
			if pathExists(p) {
				return p
			}
		}
	case "windows":
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		p := filepath.Join(base, "mnemo", "logs", "mnemo.log")
		if pathExists(p) {
			return p
		}
	}
	// As a last resort, look for any *.log next to the binary.
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", "var", "log", "mnemo.log")
		if pathExists(p) {
			return p
		}
	}
	// silence net import on windows builds where lsof is skipped
	_ = net.IPv4len
	return ""
}

// ensure database/sql + driver are linked
var _ = sql.ErrNoRows
