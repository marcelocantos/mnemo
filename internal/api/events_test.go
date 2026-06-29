// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestEventHubFanOut(t *testing.T) {
	hub := NewEventHub()
	if hub.HasSubscribers() {
		t.Fatal("empty hub reports subscribers")
	}
	_, ch1 := hub.subscribe()
	_, ch2 := hub.subscribe()
	if !hub.HasSubscribers() {
		t.Fatal("hub with subscribers reports none")
	}
	hub.Publish(Event{Type: "health", Data: map[string]int{"ok": 3}})
	for i, ch := range []chan Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.Type != "health" {
				t.Errorf("subscriber %d: type = %q, want health", i, ev.Type)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: no event delivered", i)
		}
	}
}

// A retained event published before a client connects is replayed to it on
// subscribe; a transient (Publish) event is not.
func TestEventHubRetention(t *testing.T) {
	hub := NewEventHub()
	hub.PublishRetained(Event{Type: "health", Data: map[string]int{"ok": 5}})
	hub.Publish(Event{Type: "alert", Data: map[string]string{"name": "x"}})

	_, ch := hub.subscribe()
	select {
	case ev := <-ch:
		if ev.Type != "health" {
			t.Fatalf("replayed event type = %q, want health", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("retained event not replayed on subscribe")
	}
	// No second event: the transient alert was not retained.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected extra event %q (transient should not replay)", ev.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventHubUnsubscribe(t *testing.T) {
	hub := NewEventHub()
	id, _ := hub.subscribe()
	hub.unsubscribe(id)
	if hub.HasSubscribers() {
		t.Fatal("hub still reports subscribers after unsubscribe")
	}
}

// A slow (never-drained) subscriber must not stall the publisher: once its
// buffer fills, further events for it are dropped, not blocked on.
func TestEventHubNonBlocking(t *testing.T) {
	hub := NewEventHub()
	_, _ = hub.subscribe() // never drained
	done := make(chan struct{})
	go func() {
		for range 1000 {
			hub.Publish(Event{Type: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

// The SSE endpoint streams published events with correct event/data framing.
func TestEventStreamFraming(t *testing.T) {
	h := New(func(string) (store.Backend, error) { return nil, nil })
	hub := NewEventHub()
	h.SetEventHub(hub)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Publish repeatedly until the reader sees it — sidesteps the race between
	// the client receiving headers and the handler registering its subscriber.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				hub.Publish(Event{Type: "health", Data: map[string]int{"ok": 1}})
			}
		}
	}()

	sc := bufio.NewScanner(resp.Body)
	var sawEvent, sawData bool
	deadline := time.AfterFunc(3*time.Second, cancel)
	defer deadline.Stop()
	for sc.Scan() {
		switch line := sc.Text(); {
		case line == "event: health":
			sawEvent = true
		case strings.HasPrefix(line, "data: ") && strings.Contains(line, `"ok":1`):
			sawData = true
		}
		if sawEvent && sawData {
			break
		}
	}
	if !sawEvent || !sawData {
		t.Fatalf("missing SSE framing: event=%v data=%v", sawEvent, sawData)
	}
}
