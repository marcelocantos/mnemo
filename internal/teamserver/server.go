// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package teamserver runs the central team-mnemo instance (🎯T36.1,
// 🎯T36.3): an mTLS listener that accepts contributor pushes on /push
// and serves author-aware, read-only MCP tools on /mcp.
//
// It is a THIRD listener, deliberately not folded into either of the
// existing two. :19419 is local and unauthenticated; :19420 is
// federation, whose peers are machines the same person owns. :19421 is
// the only surface where the caller is another human being, where a
// write is accepted, and where stored content belongs to someone other
// than the daemon's owner. Sharing a port with either of the others
// would mean one authorisation mistake could expose the local index to
// the whole team, or the team index to an unauthenticated localhost
// caller.
//
// The same binary serves all three. There is no "central mode" flag:
// what makes an instance central is that contributors push to it, which
// is a fact about how it is used, not a mode it is put into.
package teamserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/teampush"
)

// DefaultAddr is the team push+query listen address from the design's
// port table: :19419 local, :19420 federated, :19421 team.
const DefaultAddr = ":19421"

// Index is the subset of the store the team server needs. Narrow on
// purpose: the team server must not be able to reach the single-user
// read paths even by accident, and an interface this small makes that
// checkable by reading the type rather than by auditing call sites.
type Index interface {
	IngestTeamPush(author string, sess store.TeamPushedSession, now time.Time) (store.TeamIngestResult, error)
	RecordTeamPush(author, peerName string, now time.Time, sessions, accepted, skipped, conflicts, entries int) error
	CountTeamPushedSessionsSince(author string, cutoff time.Time) (int, error)
	SearchTeam(p store.TeamSearchParams) ([]store.TeamSearchHit, error)
	ListTeamSessions(p store.TeamSessionsParams) ([]store.TeamSessionInfo, error)
	ReadTeamSession(sessionID string, offset, limit int) ([]store.TeamSearchHit, error)
	TeamRecentActivity(days int, repoFilter, authorFilter string) ([]store.TeamActivity, error)
	TeamAuthors() ([]store.TeamActivity, error)
	TeamStats() (*store.TeamIndexStats, error)
	RetractTeamSession(sessionID, requester string, admin bool) (int, error)
}

// Config tunes the team server.
type Config struct {
	// Addr is the listen address. Empty disables the team server
	// entirely — which is the default, because most mnemo installs are
	// one person's laptop and should not open an authenticated write
	// port to the network without being asked.
	Addr string

	// RateLimitSessions bounds how many sessions one contributor may
	// push per RateLimitWindow. Zero uses DefaultRateLimitSessions;
	// negative disables the limit.
	RateLimitSessions int

	// RateLimitWindow is the rate limit's rolling window. Zero uses
	// DefaultRateLimitWindow.
	RateLimitWindow time.Duration

	// MaxBodyBytes caps a single push request body. Zero uses
	// DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// Admins are peer names permitted to retract other contributors'
	// sessions. Empty means nobody can, which is the safe default: an
	// admin list is a decision the operator makes, not one the software
	// makes for them.
	Admins []string
}

// Defaults for Config.
const (
	// DefaultRateLimitSessions is the design's "e.g. 100 sessions per
	// hour". It is a blast-radius bound on a stolen cert, not a
	// capacity limit — a contributor's first push after onboarding is
	// the only legitimate case that comes near it, and that one is
	// worth a second run.
	DefaultRateLimitSessions = 100

	// DefaultRateLimitWindow pairs with DefaultRateLimitSessions.
	DefaultRateLimitWindow = time.Hour

	// DefaultMaxBodyBytes caps a push at 64 MiB of JSON.
	DefaultMaxBodyBytes = 64 << 20
)

// Server is a running team-mnemo instance.
type Server struct {
	cfg   Config
	ep    *endpoint.Endpoint
	index Index
	http  *http.Server

	admins map[string]bool
}

// Start brings up the team listener. mnemoDir supplies the mTLS
// material; version is reported in MCP metadata. Returns the running
// server, or an error the caller may log and continue from — a team
// endpoint that fails to bind must not take the local daemon down with
// it.
func Start(ctx context.Context, mnemoDir, version string, index Index, cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("team server: no listen address")
	}
	ep, err := endpoint.Load(mnemoDir)
	if err != nil {
		return nil, fmt.Errorf("load endpoint: %w", err)
	}
	tlsCfg, err := ep.ServerTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("build server TLS config: %w", err)
	}

	s := &Server{cfg: cfg, ep: ep, index: index, admins: map[string]bool{}}
	for _, a := range cfg.Admins {
		s.admins[a] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc(teampush.PushPath, s.handlePush)
	// One handler instance for both paths. Building two would give the
	// trailing-slash form its own MCP server with its own session
	// state, so a client that initialised on /mcp and then posted to
	// /mcp/ would be talking to a server that had never seen it.
	mcpHandler := s.mcpHandler(version)
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	mux.HandleFunc("/health", s.handleHealth)

	s.http = &http.Server{
		Addr:      cfg.Addr,
		Handler:   mux,
		TLSConfig: tlsCfg,
		// A push can be large and a contributor's uplink slow, so the
		// read timeout is generous; the body cap, not the clock, is
		// what bounds the work.
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      5 * time.Minute,
	}

	ln, err := tls.Listen("tcp", cfg.Addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}

	slog.Info("mnemo team serve starting",
		"addr", cfg.Addr, "trusted_peers", ep.PeerNames, "admins", cfg.Admins)

	go func() {
		if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("team HTTP server failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return s, nil
}

// Shutdown stops the team server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.cfg.Addr }

// caller is an authenticated contributor.
type caller struct {
	// PeerName is the basename the admin filed this contributor's cert
	// under. It is the authorisation identity.
	PeerName string

	// Author is the identity content is attributed to — the validated
	// X-Mnemo-Author claim, or PeerName when none was sent.
	Author string
}

// authenticate resolves the mTLS client certificate to a trusted peer
// name, then validates the author claim against it.
//
// The claim is accepted only when it is the peer name, or the peer name
// is the claim plus a machine suffix ("alice" claimed from a cert filed
// as "alice-laptop"). That second form is what lets one contributor
// push from several machines under one identity without the admin
// having to maintain a mapping file, and it is still admin-controlled:
// a contributor cannot claim "bob" unless the admin filed their cert as
// "bob" or "bob-something". Self-asserted identity in a multi-author
// index is not identity at all — it is a text field.
func (s *Server) authenticate(r *http.Request) (caller, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return caller{}, fmt.Errorf("no client certificate presented")
	}
	var peerName string
	var cert *x509.Certificate
	for _, c := range r.TLS.PeerCertificates {
		if name := s.ep.PeerNameFor(c); name != "" {
			peerName, cert = name, c
			break
		}
	}
	if peerName == "" {
		// The handshake verified the chain, so this means the leaf is
		// not one of the installed peer files. Refuse rather than
		// inventing a name: an unattributable write is worse than a
		// rejected one.
		return caller{}, fmt.Errorf("client certificate is not an installed peer (see ~/.mnemo/peers/)")
	}
	_ = cert

	claim := strings.TrimSpace(r.Header.Get(teampush.AuthorHeader))
	if claim == "" {
		return caller{PeerName: peerName, Author: peerName}, nil
	}
	if claim == peerName || strings.HasPrefix(peerName, claim+"-") {
		return caller{PeerName: peerName, Author: claim}, nil
	}
	return caller{}, fmt.Errorf(
		"author claim %q does not match the trusted-peer name %q this certificate is filed under",
		claim, peerName)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	c, err := s.authenticate(r)
	if err != nil {
		httpError(w, http.StatusForbidden, err)
		return
	}
	stats, err := s.index.TeamStats()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"you":    c.Author,
		"index":  stats,
	})
}

// handlePush is the write side of team-mnemo.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("push requires POST, got %s", r.Method))
		return
	}
	c, err := s.authenticate(r)
	if err != nil {
		slog.Warn("team push rejected", "reason", "authentication", "err", err)
		httpError(w, http.StatusForbidden, err)
		return
	}

	maxBody := s.cfg.MaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultMaxBodyBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req teampush.PushRequest
	dec := json.NewDecoder(r.Body)
	// Unknown fields are refused rather than dropped. A client that
	// sends a field this server does not know is a client whose privacy
	// filtering this server cannot reason about, and silently ignoring
	// the extra data is how content nobody meant to publish gets stored.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode push: %w", err))
		return
	}
	// Validated on the server even though the client validates too: a
	// client-side check binds only clients that run it.
	if err := req.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}

	if err := s.checkRateLimit(c.Author, len(req.Sessions)); err != nil {
		slog.Warn("team push rejected", "reason", "rate_limit", "author", c.Author, "err", err)
		httpError(w, http.StatusTooManyRequests, err)
		return
	}

	now := time.Now()
	resp := teampush.PushResponse{Author: c.Author}
	for i := range req.Sessions {
		sess := &req.Sessions[i]
		res, err := s.index.IngestTeamPush(c.Author, toStoreSession(sess), now)
		if err != nil {
			slog.Error("team push ingest failed",
				"author", c.Author, "session", sess.SessionID, "err", err)
			httpError(w, http.StatusInternalServerError,
				fmt.Errorf("ingest %s: %w", sess.SessionID, err))
			return
		}
		switch res.Status {
		case store.TeamPushAccepted:
			resp.Accepted++
			resp.Entries += res.Accepted
		case store.TeamPushSkipped:
			resp.Skipped++
		case store.TeamPushConflict:
			resp.Conflicts++
			slog.Warn("team push conflict — session UUID already belongs to another contributor",
				"author", c.Author, "session", sess.SessionID, "reason", res.Reason)
		}
		resp.Sessions = append(resp.Sessions, teampush.SessionOutcome{
			SessionID: res.SessionID,
			Status:    res.Status,
			Accepted:  res.Accepted,
			Reason:    res.Reason,
		})
	}

	if err := s.index.RecordTeamPush(c.Author, c.PeerName, now,
		len(req.Sessions), resp.Accepted, resp.Skipped, resp.Conflicts, resp.Entries); err != nil {
		// The content landed; only the audit row failed. Log loudly and
		// still answer success — telling the contributor their push
		// failed would have them push again, which is worse.
		slog.Error("team push audit row failed", "author", c.Author, "err", err)
	}

	slog.Info("team push accepted",
		"author", c.Author, "peer", c.PeerName,
		"sessions", len(req.Sessions), "accepted", resp.Accepted,
		"skipped", resp.Skipped, "conflicts", resp.Conflicts, "entries", resp.Entries)

	writeJSON(w, http.StatusOK, resp)
}

// checkRateLimit bounds one contributor's push volume.
func (s *Server) checkRateLimit(author string, incoming int) error {
	limit := s.cfg.RateLimitSessions
	if limit == 0 {
		limit = DefaultRateLimitSessions
	}
	if limit < 0 {
		return nil
	}
	window := s.cfg.RateLimitWindow
	if window == 0 {
		window = DefaultRateLimitWindow
	}
	n, err := s.index.CountTeamPushedSessionsSince(author, time.Now().Add(-window))
	if err != nil {
		// Fail OPEN, and say so. The limit exists to bound a
		// compromised cert, not to be a correctness barrier; refusing
		// every contributor's push because one COUNT query failed
		// trades a speculative harm for a certain one.
		slog.Warn("team push rate-limit check failed; allowing", "author", author, "err", err)
		return nil
	}
	if n+incoming > limit {
		return fmt.Errorf(
			"rate limit: %s has pushed %d sessions in the last %s and this push adds %d (limit %d)",
			author, n, window, incoming, limit)
	}
	return nil
}

func toStoreSession(s *teampush.Session) store.TeamPushedSession {
	out := store.TeamPushedSession{
		SessionID:      s.SessionID,
		Source:         s.Source,
		Repo:           s.Repo,
		Project:        s.Project,
		GitBranch:      s.GitBranch,
		WorkType:       s.WorkType,
		Topic:          s.Topic,
		StartedAt:      s.StartedAt,
		EndedAt:        s.EndedAt,
		RedactionTally: map[string]int(s.RedactionTally),
	}
	for _, e := range s.Entries {
		out.Entries = append(out.Entries, store.TeamPushedEntry{
			Index:       e.Index,
			Role:        e.Role,
			ContentType: e.ContentType,
			Text:        e.Text,
			Timestamp:   e.Timestamp,
			ToolName:    e.ToolName,
			ToolUseID:   e.ToolUseID,
			IsError:     e.IsError,
		})
	}
	return out
}

// mcpHandler builds the read-only, author-aware MCP surface.
func (s *Server) mcpHandler(version string) http.Handler {
	mcp := mcpserver.NewMCPServer(
		"mnemo-team",
		version,
		mcpserver.WithToolCapabilities(true),
	)
	s.registerTeamTools(mcp)
	inner := mcpserver.NewStreamableHTTPServer(mcp,
		mcpserver.WithStateful(true),
		mcpserver.WithHTTPContextFunc(s.callerContextFunc),
		mcpserver.WithHeartbeatInterval(30*time.Second),
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.authenticate(r); err != nil {
			httpError(w, http.StatusForbidden, err)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
