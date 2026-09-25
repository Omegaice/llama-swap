package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPeerConfig_ModelSettingsValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "settings for a declared model",
			yaml: `
proxy: http://127.0.0.1:9000
models:
  - model_a
modelSettings:
  model_a:
    sendAs: upstream_a
    aliases: [model_a:think]
    name: Model A
    capabilities:
      in: [text]
      out: [text]
      tools: true
      context: 32000
    metadata:
      reasoning: true
`,
		},
		{
			name: "settings for an undeclared model",
			yaml: `
proxy: http://127.0.0.1:9000
models:
  - model_a
modelSettings:
  model_b:
    name: Model B
`,
			wantErr: `modelSettings key "model_b" is not one of the peer's models`,
		},
		{
			name: "alias collides with a declared model",
			yaml: `
proxy: http://127.0.0.1:9000
models:
  - model_a
  - model_b
modelSettings:
  model_a:
    aliases: [model_b]
`,
			wantErr: `alias "model_b" is already used by model "model_b"`,
		},
		{
			name: "alias collides with another alias",
			yaml: `
proxy: http://127.0.0.1:9000
models:
  - model_a
  - model_b
modelSettings:
  model_a:
    aliases: [shared]
  model_b:
    aliases: [shared]
`,
			wantErr: `alias "shared" is already used by model "model_a"`,
		},
		{
			name: "empty alias",
			yaml: `
proxy: http://127.0.0.1:9000
models:
  - model_a
modelSettings:
  model_a:
    aliases: [""]
`,
			wantErr: "alias can not be empty",
		},
		{
			name: "invalid modality",
			yaml: `
proxy: http://127.0.0.1:9000
models:
  - model_a
modelSettings:
  model_a:
    capabilities:
      in: [telepathy]
`,
			wantErr: "invalid modality",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var peer PeerConfig
			err := yaml.Unmarshal([]byte(tc.yaml), &peer)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestPeerConfig_RoutableNamesAndUpstreamName(t *testing.T) {
	peer := PeerConfig{
		Models: []string{"model_a", "model_b"},
		ModelSettings: map[string]PeerModelSettings{
			"model_a": {SendAs: "upstream_a", Aliases: []string{"model_a:think", "model_a:nothink"}},
		},
	}

	var got []string
	for _, routable := range peer.RoutableNames() {
		got = append(got, routable.Name+"->"+peer.UpstreamName(routable.ModelID))
	}

	want := []string{
		"model_a->upstream_a",
		"model_b->model_b",
		"model_a:think->upstream_a",
		"model_a:nothink->upstream_a",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConfig_ResolvePeerModel_Alias(t *testing.T) {
	cfg := Config{
		Peers: PeerDictionaryConfig{
			"spark": {
				Models: []string{"flash"},
				ModelSettings: map[string]PeerModelSettings{
					"flash": {Aliases: []string{"flash:nothink"}},
				},
			},
		},
	}

	for _, search := range []string{"spark/flash:nothink", "flash:nothink"} {
		peerID, modelID, found := cfg.ResolvePeerModel(search)
		if !found {
			t.Fatalf("%s: expected the alias to resolve", search)
		}
		if peerID != "spark" || modelID != "flash:nothink" {
			t.Fatalf("%s: got (%s, %s)", search, peerID, modelID)
		}
	}
}

func TestValidatePeerNamespace_AliasConflictsWithLocalModel(t *testing.T) {
	cfg := Config{
		Models: map[string]ModelConfig{
			"spark/flash:nothink": {},
		},
		Peers: PeerDictionaryConfig{
			"spark": {
				Models: []string{"flash"},
				ModelSettings: map[string]PeerModelSettings{
					"flash": {Aliases: []string{"flash:nothink"}},
				},
			},
		},
	}

	err := ValidatePeerNamespace(cfg)
	if err == nil {
		t.Fatal("expected a conflict between the local model ID and the peer alias FQN")
	}
	if !strings.Contains(err.Error(), "spark/flash:nothink") {
		t.Fatalf("unexpected error: %v", err)
	}
}
