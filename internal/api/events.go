// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is a typed message published to GET /api/events subscribers (🎯T86).
// Type names the event ("health", "alert", …); Data is the payload, marshalled
// to compact JSON and sent as the SSE data field. The native menu-bar shim
// subscribes and fans each event out to a notification, the status-item glyph,
// and live UI — so a new capability is a new Type plus a new consumer, not a
// new poll loop.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// EventHub is a fan-out broadcaster for Server-Sent-Events subscribers. The
// daemon publishes typed Events; each connected client receives them on its own
// buffered channel. Publish never blocks on a slow client — a full buffer drops
// the event for that client, which re-primes from the next snapshot (the
// dashboard panel also pulls /health on open). HasSubscribers lets the
// notification path choose between an SSE alert (shim connected) and the
// osascript/notify-send fallback (headless).
type EventHub struct {
	mu   sync.Mutex
	subs map[int]chan Event
	next int
	// retained holds the latest snapshot event per type, replayed to each new
	// subscriber on connect. A scheduler tick is minutes apart, so without this
	// a client connecting between ticks would see nothing until the next one;
	// retention makes the stream self-priming. Transient events (alerts) are
	// not retained — replaying a stale alert would re-notify.
	retained map[string]Event
}

// NewEventHub returns an empty hub ready to register subscribers.
func NewEventHub() *EventHub {
	return &EventHub{subs: map[int]chan Event{}, retained: map[string]Event{}}
}

// subscribe registers a new client and returns its id and channel, seeding the
// channel with the current retained snapshots. The caller must unsubscribe when
// the connection ends.
func (h *EventHub) subscribe() (int, chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan Event, 16)
	h.subs[id] = ch
	for _, ev := range h.retained {
		select {
		case ch <- ev:
		default:
		}
	}
	return id, ch
}

// unsubscribe removes and closes a client's channel.
func (h *EventHub) unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// HasSubscribers reports whether any client is currently connected.
func (h *EventHub) HasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// Publish fans a transient event out to every current subscriber, non-blocking.
// A subscriber whose buffer is full misses this event rather than stalling the
// publisher.
func (h *EventHub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fanout(ev)
}

// PublishRetained is Publish plus caching: ev becomes the latest snapshot of its
// type and is replayed to clients that connect later. Use it for state
// snapshots (health), not transient events (alerts).
func (h *EventHub) PublishRetained(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retained[ev.Type] = ev
	h.fanout(ev)
}

// fanout delivers ev to all subscriber channels, non-blocking. The caller holds
// the mutex.
func (h *EventHub) fanout(ev Event) {
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// SetEventHub wires the SSE hub into the handler. Call once during startup
// before serving requests; without it GET /api/events reports 503.
func (h *Handler) SetEventHub(hub *EventHub) { h.events = hub }

// sseKeepalive is the idle interval at which a comment line is sent so proxies
// and the client's read loop don't treat a quiet stream as dead.
const sseKeepalive = 25 * time.Second

// eventStream serves GET /api/events as a Server-Sent-Events stream. Each
// connected client receives every published Event until it disconnects or the
// request context is cancelled. A periodic keepalive comment holds the
// connection open through idle gaps. The server sets no write timeout
// (main.go), so the stream is not torn down by the http.Server.
func (h *Handler) eventStream(w http.ResponseWriter, r *http.Request) {
	if h.events == nil {
		http.Error(w, "event hub not wired", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, ch := h.events.subscribe()
	defer h.events.unsubscribe(id)

	ping := time.NewTicker(sseKeepalive)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			// Compact JSON has no embedded newlines, so a single data line is
			// always valid SSE.
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
