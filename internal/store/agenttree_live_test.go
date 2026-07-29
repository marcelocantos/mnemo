//go:build ccusage

package store

import (
	"encoding/json"
	"testing"
)

// TestAgentTreesAgainstLiveCorpus is an exploratory run against real
// transcripts, sharing the ccusage tag because it too needs a real corpus
// and must never run in the ordinary suite.
func TestAgentTreesAgainstLiveCorpus(t *testing.T) {
	home, err := EffectiveHome()
	if err != nil {
		t.Skip(err)
	}
	s := openCorpus(t, home+"/.mnemo/mnemo.db")
	trees, err := s.AgentTrees(AgentTreeParams{Days: 4, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(trees) == 0 {
		t.Fatal("no agent trees found in four days of a corpus known to contain fan-outs")
	}
	for _, tr := range trees {
		slim := tr
		if len(slim.Nodes) > 3 {
			slim.Nodes = slim.Nodes[:3]
		}
		b, _ := json.MarshalIndent(slim, "", "  ")
		t.Logf("%s", b)
	}
}
