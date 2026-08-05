// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVoyageEmbed(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		var req voyageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "voyage-3-lite" {
			t.Errorf("model %q", req.Model)
		}
		// Return one vector per input, dims=2 for the test.
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		data := make([]item, len(req.Input))
		for i := range req.Input {
			data[i] = item{Embedding: []float32{float32(i + 1), 0.5}, Index: i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	v, err := NewVoyage(&VoyageArgs{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Dimensions: 2,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := v.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
	if len(vecs) != 2 || len(vecs[0]) != 2 {
		t.Fatalf("shape %v", vecs)
	}
	if vecs[0][0] != 1 || vecs[1][0] != 2 {
		t.Fatalf("values %v", vecs)
	}
}

func TestVoyageRequiresKey(t *testing.T) {
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("VOYAGEAI_API_KEY", "")
	_, err := NewVoyage(&VoyageArgs{})
	if err == nil {
		t.Fatal("expected error without key")
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	in := []float32{1.5, -2, 0, 3.25}
	b := PackFloat32LE(in)
	out, err := UnpackFloat32LE(b, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("idx %d: %v vs %v", i, out[i], in[i])
		}
	}
	if _, err := UnpackFloat32LE(b, 3); err == nil {
		t.Fatal("expected dims mismatch error")
	}
}

func TestSanitizeLLMLabel(t *testing.T) {
	got := sanitizeLLMLabel(`"Auth Middleware JWT Tokens Extra Words Here Please"`)
	if got != "auth middleware jwt tokens extra words" {
		t.Fatalf("got %q", got)
	}
}

func TestAnthropicLabeler(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("x-api-key") != "ak" {
			t.Errorf("key %q", r.Header.Get("x-api-key"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]string{{"type": "text", "text": "schema migration"}},
		})
	}))
	defer srv.Close()
	lab, err := NewAnthropicLabeler(&AnthropicLabelArgs{
		APIKey:     "ak",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := lab.Label(context.Background(), []string{"we migrated the schema", "db migration plan"})
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
	if got != "schema migration" {
		t.Fatalf("got %q", got)
	}
}
