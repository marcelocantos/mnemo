// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/teampush"
)

// Team-mnemo contributor commands (🎯T36.2, 🎯T36.4, 🎯T36.7).
//
// All three run on the contributor's own machine and are the only way
// transcript content reaches a team index. There is no background
// watcher and no daemon-side push: the design rejects automatic upload
// on privacy grounds, and the reasoning is that transcripts are far
// more sensitive than the telemetry that already makes people uneasy
// when it ships without being asked.

// TeamFile is the repo-level config checked into VCS so that
// `git clone` + `mnemo onboard-team` is the whole onboarding sequence.
type TeamFile struct {
	// Name identifies the team.
	Name string `json:"name"`

	// URL is the central instance's MCP endpoint.
	URL string `json:"url"`

	// PeerCertPEM is the team server's certificate, inline. Checked
	// into the repo deliberately: it is a public key, and shipping it
	// with the code is what lets a contributor pin the server on first
	// contact instead of trusting whatever answers the URL.
	PeerCertPEM string `json:"peer_cert_pem"`
}

// TeamFilePath is where onboard-team looks for the repo-level config.
const TeamFilePath = ".mnemo/team.json"

// teamPeerCertName is the basename the team server's cert is installed
// under in ~/.mnemo/peers/.
const teamPeerCertName = "team-mnemo"

func mnemoDirOrExit(cmd string) string {
	dir, err := endpoint.DefaultDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
	return dir
}

// cmdPushTeam is the contributor-facing push trigger (🎯T36.2).
func cmdPushTeam(args []string) {
	fs := flag.NewFlagSet("push-team", flag.ExitOnError)
	repo := fs.String("repo", "", "push sessions for repos matching this substring")
	session := fs.String("session", "", "push exactly this session id")
	since := fs.String("since", "", "push sessions active at or after this date (YYYY-MM-DD or RFC3339)")
	limit := fs.Int("limit", 0, "cap the number of sessions considered (0 = no cap)")
	dryRun := fs.Bool("dry-run", false, "show exactly what would be transmitted, and send nothing")
	verbose := fs.Bool("verbose", false, "with --dry-run, print the redacted text of every entry")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo push-team [--repo NAME] [--session ID] [--since DATE] [--dry-run]

Push local sessions to the team instance configured as team_instance in
~/.mnemo/config.json.

Everything leaves redacted. Secrets, environment-variable values and home
directory paths are removed on THIS machine before anything is sent, and
thinking blocks and images are never sent at all. A repo containing a
.mnemo-push-exclude file, or whose CLAUDE.md carries "mnemo: private", is
skipped whatever filter you give.

Run with --dry-run first. It performs the same collection and the same
redaction and prints what would be transmitted, without opening a
connection.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := store.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}
	// A dry run deliberately does NOT require team membership. Being
	// able to see what a push would contain before committing to one is
	// most valuable to the person who has not yet joined a team.
	if cfg.TeamInstance == nil && !*dryRun {
		fmt.Fprintln(os.Stderr, "push-team: no team_instance configured in ~/.mnemo/config.json")
		fmt.Fprintln(os.Stderr, "Run `mnemo onboard-team` in a repo that has "+TeamFilePath+", or add the block by hand.")
		os.Exit(1)
	}

	mnemoDir := mnemoDirOrExit("push-team")
	redactCfg, err := teampush.LoadRedactConfig(teampush.RedactConfigPath(mnemoDir))
	if err != nil {
		// Hard failure, never a fallback to built-ins: see
		// LoadRedactConfig's contract.
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}
	redactor, err := teampush.NewRedactor(redactCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}

	home, err := store.EffectiveHome()
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}
	s, err := store.New(
		filepath.Join(home, ".mnemo", "mnemo.db"),
		filepath.Join(home, ".claude", "projects"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: open store: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	collected, err := teampush.Collect(s, teampush.CollectOptions{
		Filter: store.TeamPushFilter{
			SessionID: *session,
			Repo:      *repo,
			Since:     normaliseSince(*since),
			Limit:     *limit,
		},
		Redactor: redactor,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}

	printCollectionSummary(collected, redactor, *dryRun, *verbose)

	pushable := teampush.Pushable(collected)
	if len(pushable) == 0 {
		fmt.Println("\nNothing to push.")
		return
	}
	if *dryRun {
		fmt.Printf("\nDry run: %d session(s) would be transmitted. Nothing was sent.\n", len(pushable))
		return
	}

	peersDir := filepath.Join(mnemoDir, "peers")
	serverCert, err := cfg.TeamInstance.ResolveTeamPeerCert(peersDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}
	ep, err := endpoint.Load(mnemoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: load endpoint: %v\n", err)
		os.Exit(1)
	}
	client, err := teampush.NewClient(ep, cfg.TeamInstance.URL, serverCert, cfg.TeamInstance.Author)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := client.Push(ctx, pushable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push-team: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nPushed to %s as %s\n", cfg.TeamInstance.Name, resp.Author)
	fmt.Printf("  %d accepted (%d entries), %d already present, %d conflicts\n",
		resp.Accepted, resp.Entries, resp.Skipped, resp.Conflicts)
	for _, o := range resp.Sessions {
		if o.Status == teampush.StatusConflict {
			fmt.Printf("  CONFLICT %s: %s\n", shortID(o.SessionID), o.Reason)
		}
	}
	if resp.Author != cfg.TeamInstance.Author && cfg.TeamInstance.Author != "" {
		fmt.Printf("\nNote: the server attributed this push to %q, not the %q in your config.\n",
			resp.Author, cfg.TeamInstance.Author)
	}
}

// printCollectionSummary renders what was collected, what was skipped
// and what the redactor removed.
//
// The skipped list is printed even when empty-handed runs would be
// quieter without it. A contributor who believes a repo is excluded and
// is wrong has a privacy problem, and the only moment they can catch it
// is here.
func printCollectionSummary(collected []teampush.Collected, r *teampush.Redactor, dryRun, verbose bool) {
	var pushCount, entryCount int
	totals := teampush.Tally{}
	var skipped []teampush.Collected

	for _, c := range collected {
		if c.SkippedReason != "" {
			skipped = append(skipped, c)
			continue
		}
		pushCount++
		entryCount += len(c.Session.Entries)
		totals.Merge(c.Session.RedactionTally)
	}

	fmt.Printf("Collected %d session(s), %d entries.\n", pushCount, entryCount)
	fmt.Printf("Redaction rules active: %s\n", strings.Join(r.RuleNames(), ", "))
	fmt.Printf("Redacted: %s\n", totals.Summary())

	if len(skipped) > 0 {
		fmt.Printf("\nSkipped %d session(s):\n", len(skipped))
		byReason := map[string][]string{}
		for _, c := range skipped {
			byReason[c.SkippedReason] = append(byReason[c.SkippedReason], c.Candidate.Repo)
		}
		reasons := make([]string, 0, len(byReason))
		for reason := range byReason {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			repos := dedupeStrings(byReason[reason])
			fmt.Printf("  %s — %s\n", reason, strings.Join(repos, ", "))
		}
	}

	if !dryRun {
		return
	}
	fmt.Println("\nWould transmit:")
	for _, c := range collected {
		if c.SkippedReason != "" {
			continue
		}
		fmt.Printf("  %s  %s  %d entries  %s..%s  redacted: %s\n",
			shortID(c.Session.SessionID), c.Session.Repo, len(c.Session.Entries),
			shortTime(c.Session.StartedAt), shortTime(c.Session.EndedAt),
			teampush.Tally(c.Session.RedactionTally).Summary())
		if !verbose {
			continue
		}
		for _, e := range c.Session.Entries {
			fmt.Printf("    [%d] %s/%s %s\n", e.Index, e.Role, e.ContentType, oneLine(e.Text, 160))
		}
	}
}

// cmdOnboardTeam joins this machine to a team (🎯T36.4).
func cmdOnboardTeam(args []string) {
	fs := flag.NewFlagSet("onboard-team", flag.ExitOnError)
	configPath := fs.String("config", TeamFilePath, "path to the repo's team config")
	author := fs.String("author", "", "identity to attribute your pushes to (e.g. your GitHub username)")
	check := fs.Bool("check", false, "validate connectivity to the configured team instance and exit")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo onboard-team [--config PATH] [--author NAME] [--check]

Join the team whose config lives at .mnemo/team.json in the current repo:
install the team server's certificate under ~/.mnemo/peers/, write the
team_instance block into ~/.mnemo/config.json, check that the instance is
reachable, and print your own certificate for the admin to install.

Onboarding is two-sided by design. This command gets you to the point of
being able to reach the server; you are not able to push until an admin
files the certificate it prints under ~/.mnemo/peers/ on the server, which
is what makes your identity there theirs to grant rather than yours to
claim.

--check re-runs only the connectivity test against an already-configured
team instance.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	mnemoDir := mnemoDirOrExit("onboard-team")
	ep, err := endpoint.Load(mnemoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: load endpoint: %v\n", err)
		os.Exit(1)
	}

	if *check {
		cfg, err := store.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "onboard-team: %v\n", err)
			os.Exit(1)
		}
		if cfg.TeamInstance == nil {
			fmt.Fprintln(os.Stderr, "onboard-team: no team_instance configured")
			os.Exit(1)
		}
		cert, err := cfg.TeamInstance.ResolveTeamPeerCert(filepath.Join(mnemoDir, "peers"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "onboard-team: %v\n", err)
			os.Exit(1)
		}
		reportReachability(ep, cfg.TeamInstance.URL, cert, cfg.TeamInstance.Author)
		return
	}

	raw, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: read %s: %v\n", *configPath, err)
		fmt.Fprintln(os.Stderr, "Run this from the root of a repo whose team has published "+TeamFilePath+".")
		os.Exit(1)
	}
	var tf TeamFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: parse %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	if tf.URL == "" || tf.PeerCertPEM == "" {
		fmt.Fprintf(os.Stderr, "onboard-team: %s needs both url and peer_cert_pem\n", *configPath)
		os.Exit(1)
	}
	if tf.Name == "" {
		tf.Name = "team"
	}

	peersDir := filepath.Join(mnemoDir, "peers")
	if err := os.MkdirAll(peersDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: %v\n", err)
		os.Exit(1)
	}
	certPath := filepath.Join(peersDir, teamPeerCertName+".pem")
	if err := os.WriteFile(certPath, []byte(tf.PeerCertPEM), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: write %s: %v\n", certPath, err)
		os.Exit(1)
	}
	fmt.Printf("Installed the team server certificate at %s\n", certPath)

	ti := store.TeamInstance{
		Name:     tf.Name,
		URL:      tf.URL,
		PeerCert: teamPeerCertName,
		Author:   *author,
	}
	if err := writeTeamInstanceConfig(mnemoDir, ti); err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote team_instance %q → %s into %s\n",
		ti.Name, ti.URL, filepath.Join(mnemoDir, "config.json"))

	// Reload so the pinned cert we just wrote is the one we dial with.
	if ep, err = endpoint.Load(mnemoDir); err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: reload endpoint: %v\n", err)
		os.Exit(1)
	}
	cert, err := ti.ResolveTeamPeerCert(peersDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "onboard-team: %v\n", err)
		os.Exit(1)
	}
	reportReachability(ep, ti.URL, cert, ti.Author)

	fmt.Print("\nSend this certificate to your team's mnemo admin. Until they install\n" +
		"it under ~/.mnemo/peers/ on the server, your pushes will be refused.\n\n")
	fmt.Println(strings.TrimSpace(string(ep.CertPEM)))
}

// reportReachability dials the team instance and says plainly which of
// the three situations the contributor is in. They are genuinely
// different, and "reachable but not yet trusted" is the EXPECTED state
// right after onboarding — a contributor who reads it as breakage files
// a bug instead of messaging their admin.
func reportReachability(ep *endpoint.Endpoint, url string, cert *x509.Certificate, author string) {
	client, err := teampush.NewClient(ep, url, cert, author)
	if err != nil {
		fmt.Printf("\nConnectivity: could not build a client — %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res := client.Ping(ctx)
	switch res.Reachability {
	case teampush.Ready:
		fmt.Printf("\nConnectivity: reachable and trusted — you can push.\n")
		if res.Author != "" {
			fmt.Printf("The server knows you as %q; your pushes will be filed under that name.\n", res.Author)
		}
	case teampush.NotTrusted:
		fmt.Println("\nConnectivity: the server is reachable but does not trust your certificate yet.")
		fmt.Printf("That is expected before an admin installs it (%v).\n", res.Err)
	default:
		fmt.Printf("\nConnectivity: could not reach %s — %v\n", url, res.Err)
	}
}

// cmdInstallTeamHook installs the opt-in post-push git hook (🎯T36.7).
func cmdInstallTeamHook(args []string) {
	fs := flag.NewFlagSet("install-team-hook", flag.ExitOnError)
	remove := fs.Bool("remove", false, "remove a previously installed hook")
	force := fs.Bool("force", false, "overwrite an existing hook that mnemo did not write")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo install-team-hook [--remove] [--force]

Install a git hook in the current repo that runs `+"`mnemo push-team`"+` for this
repo after a successful `+"`git push`"+`.

Opt-in, per repo, and never installed for you. The hook runs in the
background and cannot fail your git push: if mnemo is missing, slow or
broken, git is unaffected.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if runtime.GOOS == "windows" {
		// Say so rather than writing a shell script that will not run.
		fmt.Fprintln(os.Stderr, "install-team-hook: git hooks here are shell scripts; on Windows run `mnemo push-team` manually or wire it into your own tooling.")
		os.Exit(1)
	}

	root, err := gitTopLevel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install-team-hook: %v\n", err)
		os.Exit(1)
	}
	hooksDir, err := gitHooksDir(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "install-team-hook: %v\n", err)
		os.Exit(1)
	}
	// git has no post-push hook. pre-push is the one that fires on a
	// push, so the hook body defers the work instead of doing it inline:
	// running push-team synchronously here would make every `git push`
	// wait on a network round trip to the team server.
	hookPath := filepath.Join(hooksDir, "pre-push")

	if *remove {
		data, err := os.ReadFile(hookPath)
		if os.IsNotExist(err) {
			fmt.Println("No hook installed.")
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "install-team-hook: %v\n", err)
			os.Exit(1)
		}
		if !strings.Contains(string(data), hookMarker) {
			fmt.Fprintf(os.Stderr, "install-team-hook: %s was not written by mnemo; leaving it alone\n", hookPath)
			os.Exit(1)
		}
		if err := os.Remove(hookPath); err != nil {
			fmt.Fprintf(os.Stderr, "install-team-hook: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed %s\n", hookPath)
		return
	}

	if existing, err := os.ReadFile(hookPath); err == nil {
		if !strings.Contains(string(existing), hookMarker) && !*force {
			fmt.Fprintf(os.Stderr,
				"install-team-hook: %s already exists and mnemo did not write it.\n"+
					"Refusing to overwrite. Re-run with --force, or add this line to it yourself:\n\n  %s\n",
				hookPath, hookInvocation)
			os.Exit(1)
		}
	}

	repoName := filepath.Base(root)
	body := fmt.Sprintf(`#!/bin/sh
%s
#
# Pushes this repo's sessions to the configured team mnemo instance after
# a git push. Detached and silenced on purpose: a team-memory upload must
# never be able to fail, slow, or block your git push.
#
# Remove with: mnemo install-team-hook --remove
(mnemo push-team --repo %q >/dev/null 2>&1 &)
exit 0
`, hookMarker, repoName)

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install-team-hook: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(hookPath, []byte(body), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install-team-hook: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Installed %s\n", hookPath)
	fmt.Printf("Sessions for %s will be pushed after each successful git push.\n", repoName)
	fmt.Println("Run `mnemo push-team --repo " + repoName + " --dry-run` to see what that will send.")
}

// hookMarker identifies a hook mnemo wrote, so --remove and --force can
// tell it apart from the user's own.
const hookMarker = "# mnemo team-push hook (🎯T36.7)"

const hookInvocation = `(mnemo push-team --repo "$(basename "$(git rev-parse --show-toplevel)")" >/dev/null 2>&1 &)`

func gitTopLevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository")
	}
	return strings.TrimSpace(string(out)), nil
}

// gitHooksDir resolves where hooks actually live, which is not always
// .git/hooks: core.hooksPath redirects it, and in a worktree .git is a
// file pointing elsewhere. Writing to the wrong place would install a
// hook that silently never runs.
func gitHooksDir(root string) (string, error) {
	if out, err := exec.Command("git", "config", "--get", "core.hooksPath").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if filepath.IsAbs(p) {
				return p, nil
			}
			return filepath.Join(root, p), nil
		}
	}
	out, err := exec.Command("git", "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		return "", fmt.Errorf("resolve hooks dir: %w", err)
	}
	p := strings.TrimSpace(string(out))
	if filepath.IsAbs(p) {
		return p, nil
	}
	return filepath.Join(root, p), nil
}

// writeTeamInstanceConfig merges a team_instance block into
// ~/.mnemo/config.json, preserving every other key.
//
// The file is read, decoded generically, amended and rewritten, rather
// than being reconstructed from the Config struct. Config is a large
// struct with omitempty on almost every field, and a round trip through
// it would silently drop any key this binary is older or newer than.
func writeTeamInstanceConfig(mnemoDir string, ti store.TeamInstance) error {
	path := filepath.Join(mnemoDir, "config.json")
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse %s: %w (fix it by hand before onboarding)", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	block := map[string]any{
		"name":      ti.Name,
		"url":       ti.URL,
		"peer_cert": ti.PeerCert,
	}
	if ti.Author != "" {
		block["author"] = ti.Author
	}
	raw["team_instance"] = block

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(mnemoDir, 0o700); err != nil {
		return err
	}
	// Write via a temp file in the same directory: the daemon watches
	// this path and adopting a half-written config is worse than
	// adopting none.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normaliseSince accepts a bare date as well as a full timestamp, so
// `--since 2026-09-01` does what it looks like it does.
func normaliseSince(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) == len("2006-01-02") {
		return s + "T00:00:00Z"
	}
	return s
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func shortTime(ts string) string {
	if len(ts) >= 16 {
		return ts[:16]
	}
	return ts
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
