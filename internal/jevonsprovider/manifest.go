// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package jevonsprovider is mnemo's side of the Jevons provider contract
// (Jevons 🎯T27.8). mnemo dials jevonsd's /ws/provider, answers describe
// with a stable manifest (health feed + status UI + MCP endpoint), and
// streams health snapshots so the hub's aggregated model, composed UI,
// and /mcp re-export work with zero mnemo-specific code in jevonsd.
package jevonsprovider

import "time"

// ContractMajor is the provider-contract major this peer speaks.
// Must match jevons internal/provider.ContractMajor ("1").
const ContractMajor = "1"

// Fixed capability identifiers (stable across versions).
const (
	ProviderID    = "mnemo"
	FeedHealth    = "health"
	FeedSchema    = "mnemo.health.v1"
	SurfaceStatus = "mnemo.status"
	SurfaceTitle  = "mnemo"
)

// Manifest is the describe_ok payload (Jevons provider-contract §4).
type Manifest struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	Contract     string       `json:"contract"`
	Capabilities Capabilities `json:"capabilities"`
	Egress       bool         `json:"egress"`
}

// Capabilities lists optional contribution paths.
type Capabilities struct {
	Feeds []FeedCap    `json:"feeds,omitempty"`
	UI    []UISurface  `json:"ui,omitempty"`
	MCP   *MCPEndpoint `json:"mcp,omitempty"`
}

// FeedCap describes one append-only event stream.
type FeedCap struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
	Replay bool   `json:"replay"`
}

// UISurface is one declarative ViewNode panel.
type UISurface struct {
	Surface string    `json:"surface"`
	Title   string    `json:"title"`
	Feeds   []string  `json:"feeds,omitempty"`
	Root    *ViewNode `json:"root,omitempty"`
}

// MCPEndpoint is where the hub MCP client dials.
type MCPEndpoint struct {
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// ViewNode matches the Jevons/iOS ViewNode wire shape.
type ViewNode struct {
	Type     string         `json:"type"`
	ID       string         `json:"id,omitempty"`
	Props    map[string]any `json:"props,omitempty"`
	Children []ViewNode     `json:"children,omitempty"`
}

// FeedEvent is one append-only feed record.
type FeedEvent struct {
	Feed   string         `json:"feed"`
	Seq    int64          `json:"seq"`
	TS     time.Time      `json:"ts"`
	Origin string         `json:"origin"`
	Kind   string         `json:"kind"`
	Data   map[string]any `json:"data,omitempty"`
}

// BuildManifest returns the stable mnemo provider describe payload.
// mcpEndpoint is the streamable-HTTP MCP URL (usually http://host:port/mcp).
func BuildManifest(version, mcpEndpoint string) Manifest {
	root := StatusRoot("starting")
	m := Manifest{
		ID:       ProviderID,
		Version:  version,
		Contract: ContractMajor,
		Capabilities: Capabilities{
			Feeds: []FeedCap{{
				Name:   FeedHealth,
				Schema: FeedSchema,
				Replay: true,
			}},
			UI: []UISurface{{
				Surface: SurfaceStatus,
				Title:   SurfaceTitle,
				Feeds:   []string{FeedHealth},
				Root:    &root,
			}},
		},
		Egress: false,
	}
	if mcpEndpoint != "" {
		m.Capabilities.MCP = &MCPEndpoint{
			Transport: "http",
			Endpoint:  mcpEndpoint,
		}
	}
	return m
}

// StatusRoot is the golden ViewNode for surface mnemo.status.
// statusText is a short health line shown in the panel.
func StatusRoot(statusText string) ViewNode {
	if statusText == "" {
		statusText = "mnemo"
	}
	return ViewNode{
		Type: "vstack",
		ID:   "mnemo.root",
		Props: map[string]any{
			"spacing": 8,
		},
		Children: []ViewNode{
			{
				Type: "text",
				ID:   "mnemo.title",
				Props: map[string]any{
					"text":  "mnemo",
					"font":  "headline",
					"color": "primary",
				},
			},
			{
				Type: "text",
				ID:   "mnemo.health",
				Props: map[string]any{
					"text": statusText,
					"font": "body",
				},
			},
			{
				Type: "button",
				ID:   "mnemo.refresh",
				Props: map[string]any{
					"text":   "Refresh",
					"action": "refresh",
					"style":  "bordered",
				},
			},
		},
	}
}
