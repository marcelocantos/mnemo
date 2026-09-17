// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package teampush

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestRedactor(t *testing.T) *Redactor {
	t.Helper()
	r, err := NewRedactor(RedactConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestBuiltinSecretPatterns is the acceptance check for the pattern set
// named in the design. Each case asserts BOTH that the secret is gone
// and that the rule that removed it is the specifically-named one — a
// token swallowed by the generic entropy sweep is still redacted, but
// the contributor's audit trail then says "high_entropy" where it could
// have said "github_token".
func TestBuiltinSecretPatterns(t *testing.T) {
	r := newTestRedactor(t)
	cases := []struct {
		name   string
		in     string
		secret string
		rule   string
	}{
		{
			name:   "anthropic",
			in:     "export ANTHROPIC_API_KEY=sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFF1234",
			secret: "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFF1234",
			rule:   "anthropic_api_key",
		},
		{
			name:   "github classic",
			in:     "using token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 for auth",
			secret: "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
			rule:   "github_token",
		},
		{
			name:   "github fine-grained",
			in:     "github_pat_11ABCDEFG0aBcDeFgHiJkLmNoPqRsTuVwXyZ012345 is the pat",
			secret: "github_pat_11ABCDEFG0aBcDeFgHiJkLmNoPqRsTuVwXyZ012345",
			rule:   "github_token",
		},
		{
			name:   "aws",
			in:     "aws_access_key_id AKIAIOSFODNN7EXAMPLE",
			secret: "AKIAIOSFODNN7EXAMPLE",
			rule:   "aws_access_key",
		},
		{
			name:   "bearer",
			in:     "curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9'",
			secret: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			rule:   "bearer_token",
		},
		{
			name: "private key block",
			in: "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
				"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB\n" +
				"-----END OPENSSH PRIVATE KEY-----",
			secret: "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB",
			rule:   "private_key_block",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, tally := r.Redact(tc.in)
			if strings.Contains(out, tc.secret) {
				t.Errorf("secret survived redaction:\n in: %s\nout: %s", tc.in, out)
			}
			if tally[tc.rule] == 0 {
				t.Errorf("rule %q did not fire; tally was %s", tc.rule, tally.Summary())
			}
		})
	}
}

// TestEnvValueKeepsTheName checks the asymmetry the rule is built on:
// that a variable was set is context worth having, what it was set to
// is the leak.
func TestEnvValueKeepsTheName(t *testing.T) {
	r := newTestRedactor(t)
	out, tally := r.Redact("DATABASE_URL=postgres://user:hunter2@db.internal/app")
	if !strings.HasPrefix(out, "DATABASE_URL=") {
		t.Errorf("variable name was removed: %s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("value survived: %s", out)
	}
	if tally["env_value"] != 1 {
		t.Errorf("env_value tally = %d, want 1 (%s)", tally["env_value"], tally.Summary())
	}
}

// TestProseSurvives is the false-positive guard, and it is the test that
// decides whether the team index is usable. A redactor that removes
// ordinary technical prose produces transcripts nobody can read, which
// costs more than the marginal secret it catches.
func TestProseSurvives(t *testing.T) {
	r := newTestRedactor(t)
	prose := []string{
		"The compactor writes a summary once the span exceeds the threshold, " +
			"which is why the addenda tail is computed live rather than stored.",
		"go test -tags \"sqlite_fts5\" ./internal/store/ -run TestCompactedView",
		"internal/store/compress_readers_test.go:126 flagged a bare column read",
		"count=3 retries=0 elapsed=1.2s",
		"See docs/design/team-mnemo.md for the push protocol.",
	}
	for _, p := range prose {
		out, tally := r.Redact(p)
		if out != p {
			t.Errorf("prose was altered by %s:\n in: %s\nout: %s", tally.Summary(), p, out)
		}
	}
}

// TestHighEntropyCatchesUnknownSecretShapes covers the case the named
// patterns cannot: a credential from a provider nobody wrote a rule for.
func TestHighEntropyCatchesUnknownSecretShapes(t *testing.T) {
	r := newTestRedactor(t)
	// 48 chars of mixed-case base64-ish material, no recognisable prefix.
	secret := "Xq7fJ2mR9tLw4vZc1nB8dK6sY3hP5gA0eU7iO2jWqM4xTnVb"
	out, tally := r.Redact("the token is " + secret + " apparently")
	if strings.Contains(out, secret) {
		t.Errorf("high-entropy token survived: %s (tally %s)", out, tally.Summary())
	}
	if tally["high_entropy"] != 1 {
		t.Errorf("high_entropy tally = %d, want 1", tally["high_entropy"])
	}
}

// TestHighEntropySkipsPathsAndIdentifiers pins the deliberate exemption.
// A long path or dotted identifier clears the entropy bar, and redacting
// it would remove precisely the technical content team search exists for.
func TestHighEntropySkipsPathsAndIdentifiers(t *testing.T) {
	r := newTestRedactor(t)
	keep := []string{
		"/var/folders/qz/8xk2p9_d7nz1c4vr6sgtm3h80000gn/T/mnemo-summariser",
		"github.com/marcelocantos/mnemo/internal/store/compress_readers_test.go",
	}
	for _, k := range keep {
		out, _ := r.Redact(k)
		if strings.Contains(out, Placeholder) {
			t.Errorf("path/identifier was redacted: %s -> %s", k, out)
		}
	}
}

func TestHomePathMasking(t *testing.T) {
	r := newTestRedactor(t)
	out, tally := r.Redact("error reading /Users/alice/work/secret-client/main.go")
	if strings.Contains(out, "alice") {
		t.Errorf("username survived: %s", out)
	}
	if !strings.Contains(out, "~/work/secret-client/main.go") {
		t.Errorf("path structure lost: %s", out)
	}
	if tally["home_path"] != 1 {
		t.Errorf("home_path tally = %d, want 1", tally["home_path"])
	}

	linux, _ := r.Redact("/home/bob/.mnemo/config.json")
	if strings.Contains(linux, "bob") {
		t.Errorf("linux home not masked: %s", linux)
	}
}

func TestCustomPatternsAreLoadedAndNamed(t *testing.T) {
	dir := t.TempDir()
	path := RedactConfigPath(dir)
	if err := os.WriteFile(path, []byte(`
custom_patterns:
  - name: internal_host
    regex: '\b[a-z0-9-]+\.corp\.internal\b'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRedactConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRedactor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, tally := r.Redact("deployed to build07.corp.internal last night")
	if strings.Contains(out, "corp.internal") {
		t.Errorf("custom pattern did not fire: %s", out)
	}
	if tally["custom:internal_host"] != 1 {
		t.Errorf("tally = %s, want custom:internal_host 1", tally.Summary())
	}
	names := strings.Join(r.RuleNames(), ",")
	if !strings.Contains(names, "custom:internal_host") {
		t.Errorf("RuleNames omits the custom rule: %s", names)
	}
}

// TestMalformedRedactConfigIsAnError is the one that matters most for
// trust: a contributor who wrote a custom pattern to keep something off
// the wire, and typoed it, must not be told the push succeeded.
func TestMalformedRedactConfigIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := RedactConfigPath(dir)
	if err := os.WriteFile(path, []byte("custom_patterns:\n  - name: bad\n    regex: '[unclosed'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRedactConfig(path); err == nil {
		t.Fatal("malformed regex accepted; a push would silently ship what the rule was guarding")
	}

	bad := filepath.Join(dir, "not-yaml.yaml")
	if err := os.WriteFile(bad, []byte("custom_patterns: [ this is not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRedactConfig(bad); err == nil {
		t.Fatal("malformed YAML accepted")
	}
}

func TestMissingRedactConfigIsNotAnError(t *testing.T) {
	cfg, err := LoadRedactConfig(RedactConfigPath(t.TempDir()))
	if err != nil {
		t.Fatalf("absent config should mean built-ins only: %v", err)
	}
	if len(cfg.CustomPatterns) != 0 {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

// TestCustomPatternsCannotDisableBuiltins pins the additive-only rule.
// A config file that could switch a built-in off would be a way to leak
// by configuration.
func TestCustomPatternsCannotDisableBuiltins(t *testing.T) {
	r, err := NewRedactor(RedactConfig{
		CustomPatterns: []CustomPattern{{Name: "noop", Regex: `zzzznevermatches`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := r.Redact("ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	if strings.Contains(out, "ghp_") {
		t.Errorf("built-in rule was displaced by a custom config: %s", out)
	}
}

func TestTallySummaryOrdersByCount(t *testing.T) {
	tally := Tally{"env_value": 1, "github_token": 3, "aws_access_key": 3}
	// Ties break by name, so the order is deterministic across runs.
	if got, want := tally.Summary(), "aws_access_key 3, github_token 3, env_value 1"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	if tally.Total() != 7 {
		t.Errorf("Total() = %d, want 7", tally.Total())
	}
	if (Tally{}).Summary() != "none" {
		t.Error("empty tally should summarise as none")
	}
}

// TestRedactIsIdempotent matters because a re-push runs the pipeline
// over text that may already contain placeholders.
func TestRedactIsIdempotent(t *testing.T) {
	r := newTestRedactor(t)
	in := "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 at /Users/alice/x"
	once, _ := r.Redact(in)
	twice, tally := r.Redact(once)
	if once != twice {
		t.Errorf("second pass changed the text:\n1: %s\n2: %s (%s)", once, twice, tally.Summary())
	}
}
