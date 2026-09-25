package router

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// An alias and a sendAs name both reach the peer as the name it publishes,
// which is what lets a peer model carry request variants the peer itself
// would reject.
func TestPeer_ServeHTTP_AliasSendsUpstreamName(t *testing.T) {
	tests := []struct {
		name      string
		requested string
	}{
		{name: "declared model", requested: "spark/flash"},
		{name: "alias", requested: "spark/flash:nothink"},
		{name: "bare alias", requested: "flash:nothink"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamModel string
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := swaputil.ExtractModel(r)
				if err != nil {
					t.Errorf("ExtractModel: %v", err)
				} else {
					upstreamModel = data
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer testServer.Close()

			proxyURL, _ := url.Parse(testServer.URL)
			pr, err := NewPeer(config.Config{Peers: config.PeerDictionaryConfig{
				"spark": {
					Proxy:    testServer.URL,
					ProxyURL: proxyURL,
					Models:   []string{"flash"},
					ModelSettings: map[string]config.PeerModelSettings{
						"flash": {
							SendAs:  "qwen3.8-flash-next",
							Aliases: []string{"flash:nothink"},
						},
					},
				},
			}}, testLogger)
			if err != nil {
				t.Fatal(err)
			}

			if !pr.Handles(tc.requested) {
				t.Fatalf("Handles(%q) = false", tc.requested)
			}

			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"model":"`+tc.requested+`","prompt":"hello"}`),
			)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			pr.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
			}
			if upstreamModel != "qwen3.8-flash-next" {
				t.Fatalf("upstream model = %q, want qwen3.8-flash-next", upstreamModel)
			}
		})
	}
}
