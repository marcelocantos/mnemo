// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package jevonsprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// DefaultHealthPoll is how often the peer snapshots health into the feed
// when no external push arrives.
const DefaultHealthPoll = 30 * time.Second

// DefaultReconnect is the wait before redialing a dropped hub connection.
const DefaultReconnect = 5 * time.Second

// ClientArgs configures a Jevons provider peer.
type ClientArgs struct {
	// HubURL is the jevonsd feed endpoint (ws://…/ws/provider or wss://…).
	HubURL string
	// Version is mnemo's own version string for the manifest.
	Version string
	// MCPEndpoint is the streamable-HTTP MCP URL declared to the hub.
	MCPEndpoint string
	// HealthURL is polled for feed snapshots (GET, Accept: application/json).
	// Empty disables health polling (only empty start tick + action refresh).
	HealthURL string
	// HealthPoll bounds the poll cadence. Zero uses DefaultHealthPoll.
	HealthPoll time.Duration
	// Reconnect is the wait after a connection drop. Zero uses DefaultReconnect.
	Reconnect time.Duration
	// HTTPClient is used for HealthURL. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// Logger receives connect/handshake lines. Nil uses slog.Default.
	Logger *slog.Logger
	// DialTimeout bounds one WebSocket dial. Zero uses 10s.
	DialTimeout time.Duration
}

// Client is the long-lived peer that keeps mnemo registered with a Jevons hub.
type Client struct {
	args ClientArgs
	log  *slog.Logger

	mu     sync.Mutex
	seq    int64
	events []FeedEvent // bounded replay window for subscribe from:N
}

// NewClient returns a ready peer. Call Run to dial and serve.
func NewClient(args ClientArgs) *Client {
	log := args.Logger
	if log == nil {
		log = slog.Default()
	}
	if args.HealthPoll <= 0 {
		args.HealthPoll = DefaultHealthPoll
	}
	if args.Reconnect <= 0 {
		args.Reconnect = DefaultReconnect
	}
	if args.DialTimeout <= 0 {
		args.DialTimeout = 10 * time.Second
	}
	if args.HTTPClient == nil {
		args.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{args: args, log: log}
}

// Run dials the hub, serves the contract, and reconnects until ctx is done.
func (c *Client) Run(ctx context.Context) error {
	if c.args.HubURL == "" {
		return fmt.Errorf("jevonsprovider: HubURL is required")
	}
	if c.args.Version == "" {
		return fmt.Errorf("jevonsprovider: Version is required")
	}
	for {
		err := c.serveOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Warn("jevons provider: disconnected; reconnecting",
			"hub", c.args.HubURL, "err", err, "in", c.args.Reconnect)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.args.Reconnect):
		}
	}
}

// serveOnce is one connection lifetime.
func (c *Client) serveOnce(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, c.args.DialTimeout)
	conn, _, err := websocket.Dial(dctx, c.args.HubURL, nil)
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	c.log.Info("jevons provider: connected", "hub", c.args.HubURL)

	// Hub drives: describe → describe_ok → subscribe*(feeds) → event stream.
	// Also accepts action frames for UI refresh.
	readCh := make(chan frameOrErr, 4)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readCh <- frameOrErr{err: err}
				return
			}
			var f wireFrame
			if err := json.Unmarshal(data, &f); err != nil {
				c.log.Warn("jevons provider: bad frame", "err", err)
				continue
			}
			readCh <- frameOrErr{f: f}
		}
	}()

	var healthCancel context.CancelFunc
	defer func() {
		if healthCancel != nil {
			healthCancel()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-readCh:
			if msg.err != nil {
				return msg.err
			}
			switch msg.f.Op {
			case "describe":
				m := BuildManifest(c.args.Version, c.args.MCPEndpoint)
				raw, _ := json.Marshal(wireFrame{Op: "describe_ok", Manifest: &m})
				if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
					return fmt.Errorf("describe_ok: %w", err)
				}
			case "subscribe":
				if msg.f.Feed != FeedHealth {
					c.log.Warn("jevons provider: unknown feed subscribe", "feed", msg.f.Feed)
					continue
				}
				// Replay retained events with seq >= from, then start live poll.
				from := msg.f.From
				for _, ev := range c.history(from) {
					if err := c.writeEvent(ctx, conn, ev); err != nil {
						return err
					}
				}
				// Fresh snapshot so a resume still sees current health.
				if ev, err := c.appendHealth(ctx, "snapshot"); err == nil {
					if err := c.writeEvent(ctx, conn, ev); err != nil {
						return err
					}
				}
				if healthCancel != nil {
					healthCancel()
				}
				var hctx context.Context
				hctx, healthCancel = context.WithCancel(ctx)
				go c.pollHealth(hctx, conn)
			case "action":
				if msg.f.Action == "refresh" {
					if ev, err := c.appendHealth(ctx, "refresh"); err == nil {
						_ = c.writeEvent(ctx, conn, ev)
					}
				}
			default:
				c.log.Debug("jevons provider: ignore op", "op", msg.f.Op)
			}
		}
	}
}

type wireFrame struct {
	Op       string     `json:"op"`
	Manifest *Manifest  `json:"manifest,omitempty"`
	Feed     string     `json:"feed,omitempty"`
	From     int64      `json:"from,omitempty"`
	Event    *FeedEvent `json:"event,omitempty"`
	Surface  string     `json:"surface,omitempty"`
	Action   string     `json:"action,omitempty"`
	Value    string     `json:"value,omitempty"`
}

type frameOrErr struct {
	f   wireFrame
	err error
}

const historyCap = 256

func (c *Client) history(from int64) []FeedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []FeedEvent
	for _, ev := range c.events {
		if ev.Seq >= from {
			out = append(out, ev)
		}
	}
	return out
}

func (c *Client) appendHealth(ctx context.Context, kind string) (FeedEvent, error) {
	data, err := c.snapshotHealth(ctx)
	if err != nil {
		// Still emit a degraded tick so the hub sees liveness of the peer.
		data = map[string]any{"error": err.Error(), "ok": false}
		if kind == "snapshot" {
			kind = "degraded"
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	ev := FeedEvent{
		Feed:   FeedHealth,
		Seq:    c.seq,
		TS:     time.Now().UTC().Truncate(time.Second),
		Origin: ProviderID,
		Kind:   kind,
		Data:   data,
	}
	c.events = append(c.events, ev)
	if len(c.events) > historyCap {
		c.events = c.events[len(c.events)-historyCap:]
	}
	return ev, nil
}

func (c *Client) snapshotHealth(ctx context.Context) (map[string]any, error) {
	if c.args.HealthURL == "" {
		return map[string]any{"ok": true, "detail": "no health URL"}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.args.HealthURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.args.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("health status %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return map[string]any{"ok": resp.StatusCode < 400, "status": resp.StatusCode}, nil
	}
	// Compact: keep summary fields only (avoid shipping full check list every tick).
	out := map[string]any{
		"ok":   body["ok"],
		"warn": body["warn"],
		"fail": body["fail"],
	}
	if g, ok := body["generated_at"]; ok {
		out["generated_at"] = g
	}
	return out, nil
}

func (c *Client) writeEvent(ctx context.Context, conn *websocket.Conn, ev FeedEvent) error {
	raw, err := json.Marshal(wireFrame{Op: "event", Event: &ev})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func (c *Client) pollHealth(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(c.args.HealthPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ev, err := c.appendHealth(ctx, "tick")
			if err != nil {
				continue
			}
			if err := c.writeEvent(ctx, conn, ev); err != nil {
				return
			}
		}
	}
}
