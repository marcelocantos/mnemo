// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/endpoint"
	"github.com/marcelocantos/mnemo/internal/store"
)

func TestFanoutMergesAttributedResults(t *testing.T) {
	// Two stub peers, each returning a distinct mnemo_search-shaped
	// payload. Verify the merged envelope buckets local + both peers
	// with stable instance attribution and no warnings.
	clientHostDir := t.TempDir()
	clientEP := mustLoad(t, clientHostDir)

	server1 := stubServerTrusting(t, clientEP.CertPEM,
		withStubTool("mnemo_search", `{"hits":[{"id":1,"text":"alpha-from-alice"}]}`))
	t.Cleanup(server1.shutdown)

	server2 := stubServerTrusting(t, clientEP.CertPEM,
		withStubTool("mnemo_search", `{"hits":[{"id":2,"text":"beta-from-bob"}]}`))
	t.Cleanup(server2.shutdown)

	c, err := NewClient(clientEP, []store.LinkedInstance{
		{Name: "alice", URL: server1.url, PeerCert: string(server1.endpoint.CertPEM)},
		{Name: "bob", URL: server2.url, PeerCert: string(server2.endpoint.CertPEM)},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	peers, warnings := c.Fanout(ctx(t), "mnemo_search", nil)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", warnings)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peer results, want 2", len(peers))
	}
	if peers[0].Instance != "alice" || peers[1].Instance != "bob" {
		t.Errorf("peers not sorted by instance: %+v", peers)
	}

	merged, err := MergePeerResults(`{"hits":[{"id":0,"text":"local-hit"}]}`, peers, warnings)
	if err != nil {
		t.Fatalf("MergePeerResults: %v", err)
	}

	var env FanoutEnvelope
	if err := json.Unmarshal([]byte(merged), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %s", err, merged)
	}
	if !strings.Contains(string(env.Local), "local-hit") {
		t.Errorf("local missing in envelope: %s", env.Local)
	}
	if len(env.Peers) != 2 {
		t.Fatalf("envelope peers = %d, want 2", len(env.Peers))
	}
	if !strings.Contains(string(env.Peers[0].Result), "alpha-from-alice") {
		t.Errorf("alice's payload missing: %s", env.Peers[0].Result)
	}
	if !strings.Contains(string(env.Peers[1].Result), "beta-from-bob") {
		t.Errorf("bob's payload missing: %s", env.Peers[1].Result)
	}
	if len(env.Warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", env.Warnings)
	}
}

func TestFanoutGracefulDegradationOnSlowPeer(t *testing.T) {
	// Acceptance: "one daemon paused mid-query still produces a
	// response from the other within the peer timeout, with a
	// warning naming the stalled peer."
	clientHostDir := t.TempDir()
	clientEP := mustLoad(t, clientHostDir)

	fast := stubServerTrusting(t, clientEP.CertPEM,
		withStubTool("mnemo_search", `{"hits":[{"id":1,"text":"fast"}]}`))
	t.Cleanup(fast.shutdown)

	slow := stubServerTrusting(t, clientEP.CertPEM,
		withStubTool("mnemo_search", `{"hits":[]}`),
		withDelay(500*time.Millisecond),
	)
	t.Cleanup(slow.shutdown)

	c, err := NewClient(clientEP, []store.LinkedInstance{
		{Name: "fast", URL: fast.url, PeerCert: string(fast.endpoint.CertPEM)},
		{Name: "slow", URL: slow.url, PeerCert: string(slow.endpoint.CertPEM)},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	// Tighten the slow peer's timeout to under its delay.
	c.peers["slow"].timeout = 100 * time.Millisecond
	c.peers["fast"].timeout = 2 * time.Second

	start := time.Now()
	peers, warnings := c.Fanout(ctx(t), "mnemo_search", nil)
	elapsed := time.Since(start)

	if elapsed > 400*time.Millisecond {
		t.Errorf("Fanout took %v — slow peer was not bounded by its 100ms timeout", elapsed)
	}
	if len(peers) != 1 || peers[0].Instance != "fast" {
		t.Errorf("expected only fast peer in results, got %+v", peers)
	}
	if len(warnings) != 1 || warnings[0].Instance != "slow" || warnings[0].ErrorKind != "timeout" {
		t.Errorf("expected one timeout warning for slow peer, got %+v", warnings)
	}
}

func TestFanoutClassifiesErrorKinds(t *testing.T) {
	clientHostDir := t.TempDir()
	clientEP := mustLoad(t, clientHostDir)
	otherDir := t.TempDir()
	otherEP := mustLoad(t, otherDir)

	// Pointer to nowhere → ErrConnectionRefused → "connection_refused"
	addr := freeAddr(t)
	c, err := NewClient(clientEP, []store.LinkedInstance{
		{Name: "down", URL: "https://" + addr + "/mcp", PeerCert: string(otherEP.CertPEM)},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	peers, warnings := c.Fanout(ctx(t), "mnemo_search", nil)
	if len(peers) != 0 {
		t.Errorf("expected no peer results, got %+v", peers)
	}
	if len(warnings) != 1 || warnings[0].Instance != "down" {
		t.Fatalf("expected one warning for 'down', got %+v", warnings)
	}
	if warnings[0].ErrorKind != "connection_refused" {
		t.Errorf("ErrorKind = %q, want connection_refused (msg=%s)",
			warnings[0].ErrorKind, warnings[0].Message)
	}
}

func TestMergePeerResultsNoPeersIsPassThrough(t *testing.T) {
	// Backwards-compat: when no peers are configured, the envelope
	// is suppressed and the local text is returned verbatim — so an
	// agent that never enables federation sees the original schema.
	local := `{"hits":[{"id":1}]}`
	merged, err := MergePeerResults(local, nil, nil)
	if err != nil {
		t.Fatalf("MergePeerResults: %v", err)
	}
	if merged != local {
		t.Errorf("expected pass-through, got %q", merged)
	}
}

func TestMergePeerResultsIncludesWarningsEvenWithoutResults(t *testing.T) {
	// All peers offline but at least one warning means the user
	// needs to know — envelope wraps even when peers list is empty.
	merged, err := MergePeerResults(`"local-only"`, nil, []PeerWarning{
		{Instance: "alice", ErrorKind: "connection_refused", Message: "no route"},
	})
	if err != nil {
		t.Fatalf("MergePeerResults: %v", err)
	}
	var env FanoutEnvelope
	if err := json.Unmarshal([]byte(merged), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Warnings) != 1 || env.Warnings[0].Instance != "alice" {
		t.Errorf("warnings dropped: %+v", env.Warnings)
	}
	if string(env.Local) != `"local-only"` {
		t.Errorf("local mangled: %s", env.Local)
	}
}

func TestAsJSONOrStringRoundTrip(t *testing.T) {
	cases := []struct {
		name, in string
		wantJSON bool
	}{
		{"valid-json-object", `{"k":"v"}`, true},
		{"valid-json-array", `[1,2,3]`, true},
		{"plain-text", `hello world`, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := asJSONOrString(c.in)
			if c.in == "" {
				if string(out) != "null" {
					t.Errorf("empty input should be null, got %s", out)
				}
				return
			}
			if c.wantJSON && string(out) != c.in {
				t.Errorf("JSON pass-through failed: got %s, want %s", out, c.in)
			}
			if !c.wantJSON {
				var s string
				if err := json.Unmarshal(out, &s); err != nil {
					t.Errorf("non-JSON should be wrapped as string, got %s", out)
				} else if s != c.in {
					t.Errorf("string round-trip mismatch: got %q, want %q", s, c.in)
				}
			}
		})
	}
}

// mustLoad is a t.Helper for tests that just need an Endpoint and
// don't care about asserting the load itself.
func mustLoad(t *testing.T, dir string) *endpoint.Endpoint {
	t.Helper()
	ep, err := endpoint.Load(dir)
	if err != nil {
		t.Fatalf("endpoint.Load(%s): %v", dir, err)
	}
	return ep
}
