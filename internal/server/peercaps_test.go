package server

import (
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// Declared capabilities fill in over what the peer publishes, field by field,
// the way a local model's block does over discovery.
func TestComposePeerRecords_MergesDeclaredCapabilities(t *testing.T) {
	published := `{"object":"list","data":[{"id":"flash","object":"model",
		"architecture":{"input_modalities":["text"],"output_modalities":["text"]},
		"capabilities":{"function_calling":true},
		"supported_parameters":["tools","tool_choice"],
		"context_length":8192,"context_window":8192,
		"meta":{"n_ctx":8192,"llamaswap":{"type":"model"}}}]}`

	compose := func(t *testing.T, settings config.PeerModelSettings) modelRecord {
		t.Helper()
		peer := config.PeerConfig{
			ProxyURL:      peerStub(t, published),
			Models:        []string{"flash"},
			ModelSettings: map[string]config.PeerModelSettings{"flash": settings},
		}
		s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}
		records := s.composePeerRecords("spark", peer, 0)
		if len(records) != 1 {
			t.Fatalf("records: got %d", len(records))
		}
		return records[0]
	}

	t.Run("declared modalities keep published tools and context", func(t *testing.T) {
		rec := compose(t, config.PeerModelSettings{
			Capabilities: config.ModelCapConfig{In: []string{"text", "image"}, Out: []string{"text"}},
		})
		if rec.Capabilities["vision"] != true || rec.Capabilities["function_calling"] != true {
			t.Fatalf("capabilities: got %v", rec.Capabilities)
		}
		if rec.ContextLength != 8192 || rec.ContextWindow != 8192 {
			t.Fatalf("context: got %d/%d", rec.ContextLength, rec.ContextWindow)
		}
		if rec.Meta["n_ctx"] != 8192 {
			t.Fatalf("n_ctx: got %v", rec.Meta["n_ctx"])
		}
		if rec.Meta["llamaswap"].(map[string]any)["peerID"] != "spark" {
			t.Fatalf("peerID lost: %v", rec.Meta)
		}
	})

	t.Run("declared context wins over published", func(t *testing.T) {
		rec := compose(t, config.PeerModelSettings{
			Capabilities: config.ModelCapConfig{Context: 4096},
		})
		if rec.ContextLength != 4096 || rec.ContextWindow != 4096 || rec.Meta["n_ctx"] != 4096 {
			t.Fatalf("context: got %d/%d n_ctx=%v", rec.ContextLength, rec.ContextWindow, rec.Meta["n_ctx"])
		}
		if rec.Capabilities["function_calling"] != true {
			t.Fatalf("published tools dropped: %v", rec.Capabilities)
		}
	})

	t.Run("metadata keeps the peer's other meta keys", func(t *testing.T) {
		rec := compose(t, config.PeerModelSettings{
			Metadata: map[string]any{"tier": "fast"},
		})
		if rec.Meta["n_ctx"] != float64(8192) {
			t.Fatalf("n_ctx dropped: %v", rec.Meta)
		}
		if rec.Meta["llamaswap"].(map[string]any)["tier"] != "fast" {
			t.Fatalf("metadata missing: %v", rec.Meta)
		}
	})

	t.Run("fallback record renders the declared context fully", func(t *testing.T) {
		peer := config.PeerConfig{
			ProxyURL: peerStub(t, `{"object":"list","data":[]}`),
			Models:   []string{"flash"},
			ModelSettings: map[string]config.PeerModelSettings{"flash": {
				Capabilities: config.ModelCapConfig{Context: 2048},
			}},
		}
		s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}
		rec := s.composePeerRecords("spark", peer, 0)[0]
		if rec.ContextWindow != 2048 || rec.Meta["n_ctx"] != 2048 {
			t.Fatalf("fallback context: %d n_ctx=%v", rec.ContextWindow, rec.Meta["n_ctx"])
		}
	})
}
