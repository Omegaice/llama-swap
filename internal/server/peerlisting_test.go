package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// peerStub serves a fixed /v1/models body as a peer would.
func peerStub(t *testing.T, body string) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing stub URL: %v", err)
	}
	return parsed
}

func recordByID(records []modelRecord, id string) (modelRecord, bool) {
	for _, rec := range records {
		if rec.ID == id {
			return rec, true
		}
	}
	return modelRecord{}, false
}

func innerMeta(t *testing.T, rec modelRecord) map[string]any {
	t.Helper()
	inner, ok := rec.Meta["llamaswap"].(map[string]any)
	if !ok {
		t.Fatalf("record %s has no meta.llamaswap: %v", rec.ID, rec.Meta)
	}
	return inner
}

// A peer that publishes nothing but an id gets its whole record from config.
func TestComposePeerRecords_SettingsFillABareListing(t *testing.T) {
	proxyURL := peerStub(t, `{"object":"list","data":[{"id":"flash","object":"model","owned_by":"sglang"}]}`)

	peer := config.PeerConfig{
		ProxyURL: proxyURL,
		Models:   []string{"flash"},
		ModelSettings: map[string]config.PeerModelSettings{
			"flash": {
				Name:        "Qwen Flash",
				Description: "served by sglang",
				Aliases:     []string{"flash:nothink"},
				Capabilities: config.ModelCapConfig{
					In: []string{"text"}, Out: []string{"text"}, Tools: true, Context: 262144,
				},
				Metadata: map[string]any{"reasoning": true},
			},
		},
	}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}

	records := s.composePeerRecords("spark", peer, 0)
	if len(records) != 1 {
		t.Fatalf("expected one record, got %d: %+v", len(records), records)
	}

	rec := records[0]
	if rec.ID != "spark/flash" {
		t.Fatalf("id: got %q", rec.ID)
	}
	if rec.Name != "spark: Qwen Flash" {
		t.Fatalf("name: got %q", rec.Name)
	}
	if rec.Description != "served by sglang" {
		t.Fatalf("description: got %q", rec.Description)
	}
	if rec.ContextLength != 262144 {
		t.Fatalf("context_length: got %d", rec.ContextLength)
	}
	if rec.Capabilities["function_calling"] != true {
		t.Fatalf("capabilities: got %v", rec.Capabilities)
	}

	inner := innerMeta(t, rec)
	if inner["peerID"] != "spark" {
		t.Fatalf("peerID: got %v", inner["peerID"])
	}
	if inner["reasoning"] != true {
		t.Fatalf("reasoning metadata missing: %v", inner)
	}
	aliases, ok := inner["aliases"].([]any)
	if !ok || len(aliases) != 1 || aliases[0] != "spark/flash:nothink" {
		t.Fatalf("aliases: got %v", inner["aliases"])
	}
}

// A declared model the peer serves as an alias of another model stays folded
// into that model's record rather than appearing a second time.
func TestComposePeerRecords_DeclaredRemoteAliasNotDuplicated(t *testing.T) {
	proxyURL := peerStub(t, `{"object":"list","data":[
		{"id":"qwen","object":"model","name":"Qwen","meta":{"llamaswap":{"aliases":["qwen-fixed"],"type":"model"}}}
	]}`)

	peer := config.PeerConfig{
		ProxyURL: proxyURL,
		Models:   []string{"qwen", "qwen-fixed"},
	}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"t15g2": peer}}}

	records := s.composePeerRecords("t15g2", peer, 0)
	if len(records) != 1 {
		t.Fatalf("expected one record, got %d: %+v", len(records), records)
	}
	if records[0].ID != "t15g2/qwen" {
		t.Fatalf("id: got %q", records[0].ID)
	}
	if records[0].Name != "t15g2: Qwen" {
		t.Fatalf("name: got %q", records[0].Name)
	}

	aliases, ok := innerMeta(t, records[0])["aliases"].([]any)
	if !ok || len(aliases) != 1 || aliases[0] != "t15g2/qwen-fixed" {
		t.Fatalf("aliases: got %v", innerMeta(t, records[0])["aliases"])
	}
}

// A model the peer serves but the config never declared is still forwarded,
// so a llama-swap peer needs no declaration to be discoverable.
func TestComposePeerRecords_UndeclaredLiveModelForwarded(t *testing.T) {
	proxyURL := peerStub(t, `{"object":"list","data":[
		{"id":"declared","object":"model"},
		{"id":"discovered","object":"model","name":"Discovered"}
	]}`)

	peer := config.PeerConfig{ProxyURL: proxyURL, Models: []string{"declared"}}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"t15g2": peer}}}

	records := s.composePeerRecords("t15g2", peer, 0)
	if len(records) != 2 {
		t.Fatalf("expected two records, got %d: %+v", len(records), records)
	}
	rec, ok := recordByID(records, "t15g2/discovered")
	if !ok {
		t.Fatalf("discovered model missing: %+v", records)
	}
	if rec.Name != "t15g2: Discovered" {
		t.Fatalf("name: got %q", rec.Name)
	}
}

// An unreachable peer still lists its declared models, and they keep whatever
// the config declares about them.
func TestComposePeerRecords_UnreachablePeerKeepsSettings(t *testing.T) {
	// A stub that is already shut down: the fetch fails immediately rather
	// than waiting out peerListingTimeout.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	proxyURL, err := url.Parse(closed.URL)
	if err != nil {
		t.Fatalf("parsing URL: %v", err)
	}

	peer := config.PeerConfig{
		ProxyURL: proxyURL,
		Models:   []string{"flash"},
		ModelSettings: map[string]config.PeerModelSettings{
			"flash": {
				Name:         "Qwen Flash",
				Capabilities: config.ModelCapConfig{In: []string{"text"}, Out: []string{"text"}, Context: 1024},
			},
		},
	}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}

	records := s.composePeerRecords("spark", peer, 42)
	if len(records) != 1 {
		t.Fatalf("expected one record, got %d: %+v", len(records), records)
	}
	rec := records[0]
	if rec.ID != "spark/flash" || rec.Name != "spark: Qwen Flash" {
		t.Fatalf("got id %q name %q", rec.ID, rec.Name)
	}
	if rec.ContextLength != 1024 {
		t.Fatalf("context_length: got %d", rec.ContextLength)
	}
	if rec.Created != 42 {
		t.Fatalf("created: got %d", rec.Created)
	}
	if rec.Status["value"] != "unloaded" {
		t.Fatalf("status: got %v", rec.Status)
	}
}

// An unlisted peer model stays out of /v1/models.
func TestComposePeerRecords_UnlistedModelHidden(t *testing.T) {
	proxyURL := peerStub(t, `{"object":"list","data":[{"id":"flash","object":"model"}]}`)

	peer := config.PeerConfig{
		ProxyURL: proxyURL,
		Models:   []string{"flash"},
		ModelSettings: map[string]config.PeerModelSettings{
			"flash": {Unlisted: true},
		},
	}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}

	if records := s.composePeerRecords("spark", peer, 0); len(records) != 0 {
		t.Fatalf("expected no records, got %+v", records)
	}
}

// sendAs decouples the name clients use from the one the peer answers to.
func TestComposePeerRecords_SendAsClaimsTheLiveRecord(t *testing.T) {
	proxyURL := peerStub(t, `{"object":"list","data":[{"id":"dealignai/Qwen3.8","object":"model"}]}`)

	peer := config.PeerConfig{
		ProxyURL: proxyURL,
		Models:   []string{"flash"},
		ModelSettings: map[string]config.PeerModelSettings{
			"flash": {SendAs: "dealignai/Qwen3.8"},
		},
	}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}

	records := s.composePeerRecords("spark", peer, 0)
	if len(records) != 1 {
		t.Fatalf("expected the live record to be claimed, got %d: %+v", len(records), records)
	}
	if records[0].ID != "spark/flash" {
		t.Fatalf("id: got %q", records[0].ID)
	}
}

// The JSON a client actually receives keeps the declared fields.
func TestComposePeerRecords_MarshalsDeclaredFields(t *testing.T) {
	proxyURL := peerStub(t, `{"object":"list","data":[{"id":"flash","object":"model"}]}`)

	peer := config.PeerConfig{
		ProxyURL: proxyURL,
		Models:   []string{"flash"},
		ModelSettings: map[string]config.PeerModelSettings{
			"flash": {
				Name:         "Qwen Flash",
				Capabilities: config.ModelCapConfig{In: []string{"text", "image"}, Out: []string{"text"}},
				Metadata:     map[string]any{"maxTokens": 32768},
			},
		},
	}
	s := &Server{cfg: config.Config{Peers: config.PeerDictionaryConfig{"spark": peer}}}

	body, err := json.Marshal(s.composePeerRecords("spark", peer, 0)[0])
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	caps, ok := decoded["capabilities"].(map[string]any)
	if !ok || caps["vision"] != true {
		t.Fatalf("capabilities: got %v", decoded["capabilities"])
	}
	meta := decoded["meta"].(map[string]any)["llamaswap"].(map[string]any)
	if meta["maxTokens"] != float64(32768) {
		t.Fatalf("maxTokens: got %v", meta["maxTokens"])
	}
}
