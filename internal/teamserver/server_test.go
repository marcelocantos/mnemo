// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teamserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"net/http"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/teampush"
)

// host is one machine's ~/.mnemo: its identity cert and its trusted
// peers.
type host struct {
	dir string
	ep  *endpoint.Endpoint
}

func setupHost(t *testing.T, name string) *host {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ep, err := endpoint.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &host{dir: dir, ep: ep}
}

// trustAs installs src's cert into dst's peers dir under the given
// name — exactly what a team admin does when onboarding a contributor,
// and the act that turns a stranger into a named identity.
func trustAs(t *testing.T, src, dst *host, as string) {
	t.Helper()
	peersDir := filepath.Join(dst.dir, "peers")
	if err := os.MkdirAll(peersDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peersDir, as+".pem"), src.ep.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := endpoint.Load(dst.dir)
	if err != nil {
		t.Fatal(err)
	}
	dst.ep = reloaded
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func waitForListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never listened on %s", addr)
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "team.db"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.AwaitStartup()
	t.Cleanup(func() { s.Close() })
	return s
}

// teamFixture is a running central instance plus the contributor hosts
// that may talk to it.
type teamFixture struct {
	server *host
	index  *store.Store
	addr   string
	url    string
}

func startTeam(t *testing.T, cfg Config, contributors map[string]*host) *teamFixture {
	t.Helper()
	srvHost := setupHost(t, "team-server")
	for name, c := range contributors {
		trustAs(t, c, srvHost, name)
		trustAs(t, srvHost, c, "team-mnemo")
	}
	idx := newStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	addr := freeAddr(t)
	cfg.Addr = addr
	srv, err := Start(ctx, srvHost.dir, "test", idx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	})
	waitForListen(t, addr)

	return &teamFixture{
		server: srvHost,
		index:  idx,
		addr:   addr,
		url:    fmt.Sprintf("https://%s/mcp", addr),
	}
}

func pushClient(t *testing.T, fx *teamFixture, c *host, author string) *teampush.Client {
	t.Helper()
	client, err := teampush.NewClient(c.ep, fx.url, fx.server.ep.Cert, author)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func sampleSession(id, repo string, n int) teampush.Session {
	s := teampush.Session{
		Version:   teampush.ProtocolVersion,
		SessionID: id,
		Source:    "claude",
		Repo:      repo,
		StartedAt: "2026-09-01T10:00:00Z",
		EndedAt:   "2026-09-01T11:00:00Z",
	}
	for i := 0; i < n; i++ {
		s.Entries = append(s.Entries, teampush.Entry{
			Index:       i,
			Role:        "assistant",
			ContentType: "text",
			Text:        "worked on the authentication refactor",
			Timestamp:   "2026-09-01T10:30:00Z",
		})
	}
	return s
}

// TestPushRoundTrip is the acceptance check for 🎯T36.1: a trusted
// contributor pushes, the server attributes the content to them, and
// the team index holds it under that name.
func TestPushRoundTrip(t *testing.T) {
	alice := setupHost(t, "alice")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice})

	ctx := context.Background()
	resp, err := pushClient(t, fx, alice, "alice").Push(ctx,
		[]teampush.Session{sampleSession("sess-1", "myorg/backend", 3)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Author != "alice" {
		t.Errorf("attributed to %q, want alice", resp.Author)
	}
	if resp.Accepted != 1 || resp.Entries != 3 {
		t.Errorf("got %+v, want 1 session / 3 entries", resp)
	}

	hits, err := fx.index.SearchTeam(store.TeamSearchParams{Query: "authentication"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("pushed content is not searchable")
	}
	for _, h := range hits {
		if h.Author != "alice" {
			t.Errorf("hit attributed to %q, want alice", h.Author)
		}
	}
}

// TestUntrustedContributorIsRefused is the authentication boundary:
// a valid mnemo cert that no admin installed gets nowhere. This is the
// property that makes the team index's contents attributable at all.
func TestUntrustedContributorIsRefused(t *testing.T) {
	alice := setupHost(t, "alice")
	mallory := setupHost(t, "mallory")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice})
	// Mallory learns the server's cert (it is public) but the server
	// has never been given hers.
	trustAs(t, fx.server, mallory, "team-mnemo")

	_, err := pushClient(t, fx, mallory, "mallory").Push(context.Background(),
		[]teampush.Session{sampleSession("sess-evil", "myorg/backend", 1)})
	if err == nil {
		t.Fatal("an untrusted contributor pushed successfully")
	}

	stats, err := fx.index.TeamStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 0 {
		t.Errorf("untrusted push landed %d sessions", stats.Sessions)
	}
}

// TestAuthorClaimCannotImpersonate is the authorisation boundary. The
// author header is out-of-band by design, which only works if the
// server refuses a claim that the contributor's installed cert name
// does not support. Otherwise it is a free-text field that anyone with
// any trusted cert can use to file work under a colleague's name.
func TestAuthorClaimCannotImpersonate(t *testing.T) {
	alice := setupHost(t, "alice")
	bob := setupHost(t, "bob")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice, "bob": bob})

	_, err := pushClient(t, fx, bob, "alice").Push(context.Background(),
		[]teampush.Session{sampleSession("sess-forged", "myorg/backend", 1)})
	if err == nil {
		t.Fatal("bob pushed content attributed to alice")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("unhelpful rejection: %v", err)
	}
}

// TestMultipleMachinesShareOneIdentity: a contributor's laptop and
// desktop are separate certs but one person. The admin expresses that
// by filing them as alice-laptop and alice-desktop.
func TestMultipleMachinesShareOneIdentity(t *testing.T) {
	laptop := setupHost(t, "laptop")
	desktop := setupHost(t, "desktop")
	fx := startTeam(t, Config{}, map[string]*host{
		"alice-laptop":  laptop,
		"alice-desktop": desktop,
	})

	ctx := context.Background()
	for i, h := range []*host{laptop, desktop} {
		resp, err := pushClient(t, fx, h, "alice").Push(ctx,
			[]teampush.Session{sampleSession(fmt.Sprintf("sess-%d", i), "myorg/backend", 2)})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Author != "alice" {
			t.Errorf("machine %d attributed to %q, want alice", i, resp.Author)
		}
	}
	authors, err := fx.index.TeamAuthors()
	if err != nil {
		t.Fatal(err)
	}
	if len(authors) != 1 || authors[0].Author != "alice" || authors[0].Sessions != 2 {
		t.Errorf("two machines produced %+v, want one alice with 2 sessions", authors)
	}
}

// TestOmittedAuthorClaimFallsBackToPeerName: sending no header is
// legitimate and means "whatever name you know me by".
func TestOmittedAuthorClaimFallsBackToPeerName(t *testing.T) {
	alice := setupHost(t, "alice")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice})

	resp, err := pushClient(t, fx, alice, "").Push(context.Background(),
		[]teampush.Session{sampleSession("sess-1", "myorg/backend", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Author != "alice" {
		t.Errorf("attributed to %q, want the peer name alice", resp.Author)
	}
}

// TestServerRejectsThinkingBlocks: the client filters them, and the
// server refuses them anyway. A privacy rule enforced only by the
// sender binds only senders that choose to honour it.
func TestServerRejectsThinkingBlocks(t *testing.T) {
	alice := setupHost(t, "alice")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice})

	sess := sampleSession("sess-1", "myorg/backend", 1)
	sess.Entries[0].ContentType = teampush.ContentTypeThinking
	// Bypass the client's own Validate by posting the payload directly,
	// which is what a modified or older client would do.
	err := rawPush(t, fx, alice, "alice", teampush.PushRequest{
		Version:  teampush.ProtocolVersion,
		Sessions: []teampush.Session{sess},
	})
	if err == nil {
		t.Fatal("server accepted a thinking block")
	}
	if !strings.Contains(err.Error(), "never pushed") {
		t.Errorf("unexpected rejection: %v", err)
	}
}

// TestRateLimitBoundsOneContributor: the blast radius of a stolen cert
// is bounded even before anyone notices it was stolen.
func TestRateLimitBoundsOneContributor(t *testing.T) {
	alice := setupHost(t, "alice")
	fx := startTeam(t, Config{RateLimitSessions: 2}, map[string]*host{"alice": alice})

	ctx := context.Background()
	client := pushClient(t, fx, alice, "alice")
	if _, err := client.Push(ctx, []teampush.Session{
		sampleSession("s1", "myorg/backend", 1),
		sampleSession("s2", "myorg/backend", 1),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := client.Push(ctx, []teampush.Session{sampleSession("s3", "myorg/backend", 1)})
	if err == nil {
		t.Fatal("rate limit did not bite")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTeamToolsAreReachableOverMTLS is the 🎯T36.3 acceptance check:
// the team surface answers, carries author on every result, and exposes
// only the enumerated tools.
func TestTeamToolsAreReachableOverMTLS(t *testing.T) {
	alice := setupHost(t, "alice")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice})

	ctx := context.Background()
	if _, err := pushClient(t, fx, alice, "alice").Push(ctx,
		[]teampush.Session{sampleSession("sess-1", "myorg/backend", 2)}); err != nil {
		t.Fatal(err)
	}

	c := newMCPClient(ctx, t, fx, alice)
	listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range listed.Tools {
		got[tool.Name] = true
	}
	for name := range teamToolConsumers {
		if !got[name] {
			t.Errorf("team tool %q missing from the served surface", name)
		}
	}
	// No local tool may leak onto the team endpoint: a peer is a
	// different identity domain, and mnemo_query alone would expose the
	// owner's entire private index.
	for name := range got {
		if !strings.HasPrefix(name, "mnemo_team_") {
			t.Errorf("non-team tool %q is served on the team endpoint", name)
		}
	}

	res, err := c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "mnemo_team_search",
			Arguments: map[string]any{"query": "authentication"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, `"author": "alice"`) {
		t.Errorf("search result carries no author attribution:\n%s", text)
	}
}

// TestUnauthenticatedMCPIsRefused: the query side is authenticated too.
// Read access to a team's collective transcripts is not public.
func TestUnauthenticatedMCPIsRefused(t *testing.T) {
	alice := setupHost(t, "alice")
	mallory := setupHost(t, "mallory")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice})
	trustAs(t, fx.server, mallory, "team-mnemo")

	clientCfg, err := mallory.ep.PinnedClientTLSConfig(fx.server.ep.Cert)
	if err != nil {
		t.Fatal(err)
	}
	httpc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientCfg},
		Timeout:   5 * time.Second,
	}
	resp, err := httpc.Get(fx.url)
	if err != nil {
		// A handshake-level rejection is an equally good outcome.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("an untrusted caller reached the team MCP endpoint")
	}
}

// TestRetractIsAuthorScopedOverMCP exercises the one write the team
// surface permits, through the real transport.
func TestRetractIsAuthorScopedOverMCP(t *testing.T) {
	alice := setupHost(t, "alice")
	bob := setupHost(t, "bob")
	fx := startTeam(t, Config{}, map[string]*host{"alice": alice, "bob": bob})

	ctx := context.Background()
	if _, err := pushClient(t, fx, alice, "alice").Push(ctx,
		[]teampush.Session{sampleSession("sess-1", "myorg/backend", 2)}); err != nil {
		t.Fatal(err)
	}

	bobClient := newMCPClient(ctx, t, fx, bob)
	res, err := bobClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "mnemo_team_retract",
			Arguments: map[string]any{"session_id": "sess-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("bob retracted alice's session")
	}

	aliceClient := newMCPClient(ctx, t, fx, alice)
	res, err = aliceClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "mnemo_team_retract",
			Arguments: map[string]any{"session_id": "sess-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("alice could not retract her own session: %s", resultText(t, res))
	}
	stats, err := fx.index.TeamStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 0 {
		t.Errorf("retraction left %d sessions", stats.Sessions)
	}
}

// TestTeamToolLedgerMatchesTheServedSurface is the team-side ratchet,
// the same discipline internal/tools/surface.go applies locally: a tool
// cannot appear without a reviewer seeing the ledger line.
func TestTeamToolLedgerMatchesTheServedSurface(t *testing.T) {
	defined := map[string]bool{}
	for _, tool := range teamToolDefinitions() {
		defined[tool.Name] = true
		if _, ok := teamToolConsumers[tool.Name]; !ok {
			t.Errorf("tool %q is served but absent from teamToolConsumers", tool.Name)
		}
	}
	for name := range teamToolConsumers {
		if !defined[name] {
			t.Errorf("teamToolConsumers names %q, which is not served", name)
		}
	}
}

func newMCPClient(ctx context.Context, t *testing.T, fx *teamFixture, h *host) *mcpclient.Client {
	t.Helper()
	clientCfg, err := h.ep.PinnedClientTLSConfig(fx.server.ep.Cert)
	if err != nil {
		t.Fatal(err)
	}
	httpc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientCfg},
		Timeout:   10 * time.Second,
	}
	c, err := mcpclient.NewStreamableHttpClient(fx.url, transport.WithHTTPBasicClient(httpc))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "team-test", Version: "test"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// rawPush posts a payload directly, bypassing the client's own
// Validate. It stands in for an older or modified contributor build —
// the case the server's own validation exists for.
func rawPush(t *testing.T, fx *teamFixture, h *host, author string, req teampush.PushRequest) error {
	t.Helper()
	clientCfg, err := h.ep.PinnedClientTLSConfig(fx.server.ep.Cert)
	if err != nil {
		t.Fatal(err)
	}
	httpc := &http.Client{
		Transport: &http.Transport{TLSClientConfig: clientCfg},
		Timeout:   10 * time.Second,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	httpReq, err := http.NewRequest(http.MethodPost,
		teampush.PushURL(fx.url), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if author != "" {
		httpReq.Header.Set(teampush.AuthorHeader, author)
	}
	resp, err := httpc.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}
