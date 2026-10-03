package config

import "testing"

// A peer alias is a name a client can request, so tailcat.models may list it.
func TestPublicTailcatModelIDs_IncludesPeerAliases(t *testing.T) {
	cfg := Config{Peers: PeerDictionaryConfig{"p": {
		Models:        []string{"m"},
		ModelSettings: map[string]PeerModelSettings{"m": {Aliases: []string{"a"}}},
	}}}
	ids := publicTailcatModelIDs(cfg)
	for _, want := range []string{"p/m", "p/a"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("%q missing from %v", want, ids)
		}
	}
}
