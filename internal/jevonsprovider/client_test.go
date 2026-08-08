// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package jevonsprovider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marcelocantos/mnemo/internal/jevonsprovider"
)

func TestBuildManifestThreeCapabilities(t *testing.T) {
	m := jevonsprovider.BuildManifest("0.85.0", "http://127.0.0.1:19419/mcp")
	if m.ID != jevonsprovider.ProviderID {
		t.Fatalf("id=%q", m.ID)
	}
	if m.Contract != jevonsprovider.ContractMajor {
		t.Fatalf("contract=%q", m.Contract)
	}
	if m.Egress {
		t.Fatal("egress must be false (opt-in)")
	}
	if len(m.Capabilities.Feeds) != 1 || m.Capabilities.Feeds[0].Name != jevonsprovider.FeedHealth {
		t.Fatalf("feeds=%+v", m.Capabilities.Feeds)
	}
	if !m.Capabilities.Feeds[0].Replay {
		t.Fatal("health feed must advertise replay")
	}
	if len(m.Capabilities.UI) != 1 || m.Capabilities.UI[0].Surface != jevonsprovider.SurfaceStatus {
		t.Fatalf("ui=%+v", m.Capabilities.UI)
	}
	if m.Capabilities.UI[0].Root == nil || m.Capabilities.UI[0].Root.ID != "mnemo.root" {
		t.Fatalf("root=%+v", m.Capabilities.UI[0].Root)
	}
	if m.Capabilities.MCP == nil || m.Capabilities.MCP.Endpoint == "" {
		t.Fatal("mcp endpoint required")
	}
	if m.Capabilities.MCP.Transport != "http" {
		t.Fatalf("mcp transport=%q", m.Capabilities.MCP.Transport)
	}
}

func TestStatusRootShape(t *testing.T) {
	root := jevonsprovider.StatusRoot("ok 15 fail 0")
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	// Strict client-shaped decode: type/id/props/children only.
	var got struct {
		Type     string `json:"type"`
		ID       string `json:"id"`
		Props    map[string]any
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "vstack" || got.ID != "mnemo.root" {
		t.Fatalf("root=%+v", got)
	}
	if len(got.Children) != 3 {
		t.Fatalf("children=%d", len(got.Children))
	}
}

// fakeHub is a minimal /ws/provider stand-in: sends describe, expects
// describe_ok, sends subscribe, then accepts events.
func TestClientHandshakeFeedAndAction(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":3,"warn":0,"fail":0,"generated_at":"2026-08-09T00:00:00Z"}`))
	}))
	t.Cleanup(health.Close)

	hubDone := make(chan struct{})
	var events []map[string]any
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		// describe
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"op":"describe"}`))
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Errorf("read describe_ok: %v", err)
			return
		}
		var f map[string]any
		if err := json.Unmarshal(data, &f); err != nil {
			t.Errorf("parse: %v", err)
			return
		}
		if f["op"] != "describe_ok" {
			t.Errorf("op=%v", f["op"])
			return
		}
		man, _ := f["manifest"].(map[string]any)
		if man["id"] != "mnemo" {
			t.Errorf("manifest id=%v", man["id"])
			return
		}

		// subscribe health from 0
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"op":"subscribe","feed":"health","from":0}`))

		// Read at least one event (snapshot after subscribe).
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			rctx, cancel := context.WithTimeout(ctx, time.Second)
			_, data, err := conn.Read(rctx)
			cancel()
			if err != nil {
				continue
			}
			var ef map[string]any
			if err := json.Unmarshal(data, &ef); err != nil {
				continue
			}
			if ef["op"] == "event" {
				events = append(events, ef)
				break
			}
		}
		if len(events) == 0 {
			t.Error("expected at least one health event")
		}

		// action refresh → another event
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"op":"action","surface":"mnemo.status","action":"refresh"}`))
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, err = conn.Read(rctx)
		cancel()
		if err != nil {
			t.Errorf("refresh event: %v", err)
		} else {
			var ef map[string]any
			_ = json.Unmarshal(data, &ef)
			if ef["op"] != "event" {
				t.Errorf("after refresh op=%v", ef["op"])
			}
		}
		close(hubDone)
		// Hold open briefly so client Run doesn't immediately reconnect-spam.
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(hub.Close)

	wsURL := "ws" + strings.TrimPrefix(hub.URL, "http")
	client := jevonsprovider.NewClient(jevonsprovider.ClientArgs{
		HubURL:      wsURL,
		Version:     "0.85.0-test",
		MCPEndpoint: "http://127.0.0.1:19419/mcp",
		HealthURL:   health.URL,
		HealthPoll:  time.Hour, // no background ticks in this test
		Reconnect:   time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.Run(ctx) }()

	select {
	case <-hubDone:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("hub handshake timed out")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("client Run did not exit after cancel")
	}

	// Event must carry origin mnemo and feed health.
	ev, _ := events[0]["event"].(map[string]any)
	if ev["origin"] != "mnemo" || ev["feed"] != "health" {
		t.Fatalf("event=%v", ev)
	}
	data, _ := ev["data"].(map[string]any)
	if data["ok"] == nil {
		t.Fatalf("health data missing ok: %v", data)
	}
}
