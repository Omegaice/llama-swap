package server

import (
	"slices"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// The exposed-model listing matches the request gate: a peer alias is
// requestable, fully qualified and, when unambiguous, bare.
func TestTailcatExposedModelIDs_IncludesPeerAliases(t *testing.T) {
	s := &Server{cfg: config.Config{
		Tailcat: &config.TailcatConfig{Models: []string{"*"}},
		Peers: config.PeerDictionaryConfig{"p": {
			Models:        []string{"m"},
			ModelSettings: map[string]config.PeerModelSettings{"m": {Aliases: []string{"a"}}},
		}},
	}}
	ids := s.tailcatExposedModelIDs()
	for _, want := range []string{"p/m", "m", "p/a", "a"} {
		if !slices.Contains(ids, want) {
			t.Fatalf("%q missing from %v", want, ids)
		}
	}
}
