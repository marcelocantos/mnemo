// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
)

func TestValidatePluginsOK(t *testing.T) {
	cfg := Config{Plugins: []PluginEntry{
		{Name: "a", Enabled: true, Transport: PluginTransportLaunch, Command: "/bin/a"},
		{Name: "b", Enabled: false, Transport: PluginTransportConnect, URL: "http://127.0.0.1:9"},
		{Name: "c", Enabled: true, Transport: PluginTransportInProcess, Script: "~/x.js"},
	}}
	if err := cfg.validatePlugins("/home/u"); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePluginsErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "empty name",
			cfg:  Config{Plugins: []PluginEntry{{Transport: PluginTransportLaunch, Command: "x"}}},
			want: "name is required",
		},
		{
			name: "bad name",
			cfg:  Config{Plugins: []PluginEntry{{Name: "../evil", Transport: PluginTransportLaunch, Command: "x"}}},
			want: "invalid",
		},
		{
			name: "dup",
			cfg: Config{Plugins: []PluginEntry{
				{Name: "a", Transport: PluginTransportLaunch, Command: "x"},
				{Name: "a", Transport: PluginTransportLaunch, Command: "y"},
			}},
			want: "duplicate",
		},
		{
			name: "launch needs command",
			cfg:  Config{Plugins: []PluginEntry{{Name: "a", Transport: PluginTransportLaunch}}},
			want: "command is required",
		},
		{
			name: "connect needs url",
			cfg:  Config{Plugins: []PluginEntry{{Name: "a", Transport: PluginTransportConnect}}},
			want: "url is required",
		},
		{
			name: "connect bad scheme",
			cfg:  Config{Plugins: []PluginEntry{{Name: "a", Transport: PluginTransportConnect, URL: "ftp://x"}}},
			want: "http or https",
		},
		{
			name: "inprocess needs script",
			cfg:  Config{Plugins: []PluginEntry{{Name: "a", Transport: PluginTransportInProcess}}},
			want: "script is required",
		},
		{
			name: "unknown transport",
			cfg:  Config{Plugins: []PluginEntry{{Name: "a", Transport: "wasm"}}},
			want: "invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validatePlugins("")
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != "" && !containsSub(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestPluginHome(t *testing.T) {
	got := PluginHome("/Users/me", "lab")
	want := filepath.Join("/Users/me", ".mnemo", "plugins", "lab")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}
