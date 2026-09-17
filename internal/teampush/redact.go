// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package teampush implements the contributor side of team-mnemo
// (🎯T36): the wire format for a pushed session, the redaction pipeline
// that runs before anything leaves the machine, and the mTLS client
// that transmits it.
//
// The ordering is the load-bearing part. Redaction runs on the
// contributor's own daemon, against content read from the local index,
// and the redacted payload is what the transport sees. The server never
// receives unredacted content and has no way to ask for it — there is
// no "raw" mode, and the payload carries no field the redactor did not
// produce. A privacy control that the far side can switch off is not a
// privacy control.
package teampush

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Placeholder is what every redaction leaves behind. A single visible
// token, not a deletion: a reader of team-scope search results needs to
// see that something was removed, or they will read the gap as the
// contributor never having run the command.
const Placeholder = "<REDACTED>"

// RedactionRule is one named pattern in the pipeline. The name is
// reported in the push summary and by --dry-run, so a contributor can
// tell WHICH rule fired rather than just how many times something did.
// "3 redactions" is not auditable; "2 github_token, 1 env_value" is.
type RedactionRule struct {
	// Name identifies the rule in summaries. Stable — contributors
	// build habits around these strings.
	Name string

	// Pattern matches the text to remove.
	Pattern *regexp.Regexp

	// Replacement is the template handed to Regexp.ReplaceAllString, so
	// it may reference capture groups ($1). Rules that keep a prefix use
	// this to leave the variable NAME visible while removing its value:
	// knowing that ANTHROPIC_API_KEY was set is useful context, knowing
	// what it was set to is a leak.
	Replacement string
}

// builtinRules is the pattern set the team-mnemo design enumerates, in
// application order. Order matters and is not alphabetical: the
// specific, high-confidence patterns run before the broad heuristics,
// so a GitHub token is reported as "github_token" rather than being
// swallowed by the generic high-entropy sweep that would also have
// caught it. The named rule is the better audit record.
//
// This list is the design's set and is deliberately NOT a growing
// catalogue of named third-party services. An earlier revision added
// Slack, Google and OpenAI patterns on the reasoning that more formats
// means more safety; they were removed. Generic coverage is what
// actually carries the load here — the high-entropy heuristic catches
// credential shapes no named rule anticipates, which is most of them —
// and per-vendor rules cost more than they look. The Slack fixture
// alone was realistic enough to trip GitHub's secret scanner and block
// the branch. If a format seems genuinely missing, raise it rather
// than adding it here.
var builtinRules = []RedactionRule{
	{
		Name:        "private_key_block",
		Pattern:     regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
		Replacement: Placeholder,
	},
	{
		Name:        "anthropic_api_key",
		Pattern:     regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`),
		Replacement: Placeholder,
	},
	{
		Name:        "github_token",
		Pattern:     regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{22,}`),
		Replacement: Placeholder,
	},
	{
		Name:        "aws_access_key",
		Pattern:     regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`),
		Replacement: Placeholder,
	},
	{
		Name:        "bearer_token",
		Pattern:     regexp.MustCompile(`(?i)\b(bearer|token|authorization:)\s+[A-Za-z0-9\-._~+/]{20,}={0,2}`),
		Replacement: "$1 " + Placeholder,
	},
	{
		// Environment-variable assignments in tool output. The design
		// specifies uppercase names of 4+ characters, which is narrow
		// on purpose: `PATH=...` and `HOME=...` are exactly the ambient
		// leakage this is for, while lowercase `count=3` in program
		// output is not a secret and redacting it would make transcripts
		// unreadable for no gain.
		Name:        "env_value",
		Pattern:     regexp.MustCompile(`\b([A-Z][A-Z0-9_]{3,})=([^\s"']+)`),
		Replacement: "$1=" + Placeholder,
	},
}

// homePathRe matches an absolute home-directory path belonging to ANY
// user, not just this one. The contributor's own home is the common
// case, but transcripts routinely quote paths from a colleague's
// machine (pasted stack traces, shared logs) and those identify a
// person just as well.
var homePathRe = regexp.MustCompile(`(?:/Users/|/home/|[A-Za-z]:\\Users\\)([^/\\\s"':;,)]+)`)

// Redactor applies the pipeline. Construct with NewRedactor; the zero
// value is not usable.
type Redactor struct {
	rules []RedactionRule

	// maskHome rewrites absolute home paths to ~. Configurable because
	// a team whose repos all live under a shared path prefix gains
	// nothing from it and loses the ability to search by path.
	maskHome bool

	// entropyMin is the Shannon-entropy threshold, in bits per
	// character, above which a long unbroken token is treated as an
	// encoded secret. Zero disables the heuristic.
	entropyMin float64

	// entropyMinLen is the shortest token the entropy heuristic will
	// consider. The design says 40+ characters.
	entropyMinLen int
}

// RedactConfig is the on-disk shape of ~/.mnemo/push-redact.yaml.
//
// The file is optional and additive: it adds patterns and tunes the
// heuristics, and cannot remove a built-in rule. A contributor who
// wants less redaction than the built-ins provide is asking to leak by
// configuration, which is the failure mode the file would otherwise
// introduce.
type RedactConfig struct {
	// CustomPatterns are contributor- or team-specific regexes —
	// internal hostnames, service-account names, client identifiers.
	CustomPatterns []CustomPattern `yaml:"custom_patterns"`

	// MaskHomePaths rewrites /Users/alice/... to ~/.... Defaults to
	// true; set false to keep absolute paths.
	MaskHomePaths *bool `yaml:"mask_home_paths"`

	// EntropyThreshold is bits per character, 0 to disable the
	// high-entropy heuristic. Defaults to DefaultEntropyThreshold.
	EntropyThreshold *float64 `yaml:"entropy_threshold"`

	// EntropyMinLength is the shortest token considered by the entropy
	// heuristic. Defaults to DefaultEntropyMinLength.
	EntropyMinLength *int `yaml:"entropy_min_length"`
}

// CustomPattern is one user-supplied rule.
type CustomPattern struct {
	Name    string `yaml:"name"`
	Regex   string `yaml:"regex"`
	Replace string `yaml:"replace"`
}

const (
	// DefaultEntropyThreshold is in bits per character. Base64 of random
	// bytes sits near 6.0 and hex near 4.0; English prose over a 40-char
	// window rarely exceeds 4.2. 4.5 clears prose and catches both
	// encodings — the point where a long token stops looking like words.
	DefaultEntropyThreshold = 4.5

	// DefaultEntropyMinLength is the design's "40+ characters".
	DefaultEntropyMinLength = 40
)

// RedactConfigPath returns the conventional location of the redaction
// config for a given ~/.mnemo directory.
func RedactConfigPath(mnemoDir string) string {
	return filepath.Join(mnemoDir, "push-redact.yaml")
}

// LoadRedactConfig reads ~/.mnemo/push-redact.yaml. A missing file is
// not an error — it means "built-ins only", which is the expected state
// for most contributors.
//
// A malformed file IS an error, and callers must not fall back to the
// built-ins on one. A contributor who wrote a custom pattern to keep an
// internal hostname off the wire, and typoed it, must not be told the
// push succeeded: they would have shipped the thing they were guarding.
func LoadRedactConfig(path string) (RedactConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return RedactConfig{}, nil
	}
	if err != nil {
		return RedactConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg RedactConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return RedactConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, p := range cfg.CustomPatterns {
		if p.Name == "" {
			return RedactConfig{}, fmt.Errorf("%s: custom_patterns[%d]: name is required", path, i)
		}
		if p.Regex == "" {
			return RedactConfig{}, fmt.Errorf("%s: custom_patterns[%q]: regex is required", path, p.Name)
		}
		if _, err := regexp.Compile(p.Regex); err != nil {
			return RedactConfig{}, fmt.Errorf("%s: custom_patterns[%q]: %w", path, p.Name, err)
		}
	}
	return cfg, nil
}

// NewRedactor builds the pipeline: built-in rules first, then the
// contributor's custom patterns. Custom patterns run last so a
// team-specific rule sees text the built-ins have already cleaned,
// and so a broad custom regex cannot mask which built-in would have
// fired.
func NewRedactor(cfg RedactConfig) (*Redactor, error) {
	r := &Redactor{
		rules:         append([]RedactionRule(nil), builtinRules...),
		maskHome:      true,
		entropyMin:    DefaultEntropyThreshold,
		entropyMinLen: DefaultEntropyMinLength,
	}
	if cfg.MaskHomePaths != nil {
		r.maskHome = *cfg.MaskHomePaths
	}
	if cfg.EntropyThreshold != nil {
		r.entropyMin = *cfg.EntropyThreshold
	}
	if cfg.EntropyMinLength != nil {
		r.entropyMinLen = *cfg.EntropyMinLength
	}
	for _, p := range cfg.CustomPatterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("custom pattern %q: %w", p.Name, err)
		}
		replace := p.Replace
		if replace == "" {
			replace = Placeholder
		}
		r.rules = append(r.rules, RedactionRule{
			Name:        "custom:" + p.Name,
			Pattern:     re,
			Replacement: replace,
		})
	}
	return r, nil
}

// RuleNames lists the active rules in application order. --dry-run
// prints it, so a contributor can confirm their custom patterns were
// actually loaded before trusting a push with them.
func (r *Redactor) RuleNames() []string {
	out := make([]string, 0, len(r.rules)+2)
	for _, rule := range r.rules {
		out = append(out, rule.Name)
	}
	if r.entropyMin > 0 {
		out = append(out, "high_entropy")
	}
	if r.maskHome {
		out = append(out, "home_path")
	}
	return out
}

// Tally counts redactions by rule name across whatever was redacted.
type Tally map[string]int

// Total is the number of individual redactions across all rules.
func (t Tally) Total() int {
	n := 0
	for _, c := range t {
		n += c
	}
	return n
}

// Merge folds other into t.
func (t Tally) Merge(other Tally) {
	for name, c := range other {
		t[name] += c
	}
}

// Summary renders the tally as "github_token 2, env_value 1", ordered
// by count descending then name, or "none" when nothing fired.
func (t Tally) Summary() string {
	if len(t) == 0 {
		return "none"
	}
	type kv struct {
		name string
		n    int
	}
	pairs := make([]kv, 0, len(t))
	for name, n := range t {
		pairs = append(pairs, kv{name, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].name < pairs[j].name
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("%s %d", p.name, p.n)
	}
	return strings.Join(parts, ", ")
}

// Redact applies the full pipeline to s, returning the cleaned text and
// a per-rule tally of what fired.
func (r *Redactor) Redact(s string) (string, Tally) {
	tally := Tally{}
	if s == "" {
		return s, tally
	}
	for _, rule := range r.rules {
		matches := rule.Pattern.FindAllStringIndex(s, -1)
		if len(matches) == 0 {
			continue
		}
		tally[rule.Name] += len(matches)
		s = rule.Pattern.ReplaceAllString(s, rule.Replacement)
	}
	if r.entropyMin > 0 {
		var n int
		s, n = r.redactHighEntropy(s)
		if n > 0 {
			tally["high_entropy"] += n
		}
	}
	if r.maskHome {
		var n int
		s, n = maskHomePaths(s)
		if n > 0 {
			tally["home_path"] += n
		}
	}
	return s, tally
}

// tokenSepRe splits text into candidate tokens for the entropy pass.
// Splitting on whitespace and quoting alone would keep `key: <secret>`
// attached to its punctuation and depress the measured entropy; the
// separators here are the ones that delimit a value in the formats
// transcripts actually carry (JSON, YAML, shell, URLs).
var tokenSepRe = regexp.MustCompile(`[\s"'` + "`" + `<>(){}\[\],;:=|]+`)

// redactHighEntropy removes long tokens whose character distribution
// looks encoded rather than written.
//
// It deliberately skips tokens containing a path separator or a dot.
// A 60-character file path or dotted package name clears the entropy
// bar comfortably, and redacting those would gut exactly the technical
// content the team index exists to make searchable — the false-positive
// cost here is much higher than the marginal recall.
func (r *Redactor) redactHighEntropy(s string) (string, int) {
	var b strings.Builder
	b.Grow(len(s))
	count := 0
	last := 0
	for _, loc := range tokenSepRe.FindAllStringIndex(s, -1) {
		tok := s[last:loc[0]]
		if r.isHighEntropy(tok) {
			b.WriteString(Placeholder)
			count++
		} else {
			b.WriteString(tok)
		}
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	tok := s[last:]
	if r.isHighEntropy(tok) {
		b.WriteString(Placeholder)
		count++
	} else {
		b.WriteString(tok)
	}
	return b.String(), count
}

func (r *Redactor) isHighEntropy(tok string) bool {
	if len(tok) < r.entropyMinLen {
		return false
	}
	if strings.ContainsAny(tok, "/\\.") {
		return false
	}
	if tok == Placeholder {
		return false
	}
	return shannonEntropy(tok) >= r.entropyMin
}

// shannonEntropy returns the per-character entropy of s in bits.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]int{}
	n := 0
	for _, c := range s {
		freq[c]++
		n++
	}
	var h float64
	for _, c := range freq {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

// maskHomePaths rewrites /Users/alice/... and /home/alice/... to
// ~/..., removing the username without destroying the path structure
// that makes the surrounding text comprehensible.
func maskHomePaths(s string) (string, int) {
	count := 0
	out := homePathRe.ReplaceAllStringFunc(s, func(m string) string {
		count++
		return "~"
	})
	return out, count
}

// RedactReader applies the pipeline line by line, for streaming callers.
// Used by --dry-run rendering of large tool results, where holding the
// whole payload in memory to show a preview is unnecessary.
func (r *Redactor) RedactReader(f *os.File) (string, Tally, error) {
	tally := Tally{}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line, t := r.Redact(sc.Text())
		tally.Merge(t)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", tally, err
	}
	return b.String(), tally, nil
}
