package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamChainServer wires a server to one peer that answers /v1/models with
// the given listing and echoes the path of every other request.
func upstreamChainServer(t *testing.T, listing string) (*Server, *atomic.Int32) {
	t.Helper()
	var listings atomic.Int32
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			listings.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, listing)
			return
		}
		io.WriteString(w, r.URL.EscapedPath())
	}))
	t.Cleanup(peerSrv.Close)
	proxyURL, err := url.Parse(peerSrv.URL)
	require.NoError(t, err)

	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"p": {Proxy: proxyURL.String(), ProxyURL: proxyURL, Models: []string{"foo"}},
	}}
	peer, err := router.NewPeer(cfg, logmon.NewWriter(io.Discard))
	require.NoError(t, err)
	s := newTestServerWithConfig(cfg, newStubRouter(nil, ""), peer)
	return s, &listings
}

func getUpstream(s *Server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// A peer whose listing carries llama-swap's metadata is another llama-swap,
// so /upstream/<peer>/<model>/... reaches its /upstream/<model>/... rather
// than its root, for declared models, forwarded-only ones, and their aliases.
func TestHandleUpstream_ChainsThroughLlamaSwapPeer(t *testing.T) {
	s, _ := upstreamChainServer(t, `{"object":"list","data":[
		{"id":"foo","meta":{"llamaswap":{"type":"model"}}},
		{"id":"bar/baz","meta":{"llamaswap":{"type":"model","aliases":["bz"]}}}
	]}`)

	w := getUpstream(s, "/upstream/p/foo/props")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/upstream/foo/props", w.Body.String(), "declared model")

	w = getUpstream(s, "/upstream/p/bar/baz/metrics")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/upstream/bar/baz/metrics", w.Body.String(), "forwarded-only model with a slash")

	w = getUpstream(s, "/upstream/p/bz/v1/models")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/upstream/bz/v1/models", w.Body.String(), "forwarded-only alias")

	w = getUpstream(s, "/upstream/p/bar/baz")
	assert.Equal(t, http.StatusMovedPermanently, w.Code)
	assert.Equal(t, "/upstream/p/bar/baz/", w.Header().Get("Location"), "forwarded-only name redirects like a declared one")

	w = getUpstream(s, "/upstream/p/nope/props")
	assert.Equal(t, http.StatusNotFound, w.Code, "a name the peer does not list")
}

// A peer that is not llama-swap serves its routes at its root, so the prefix
// is still stripped; its listing still decides where an undeclared model
// name ends.
func TestHandleUpstream_PlainPeerKeepsRoot(t *testing.T) {
	s, _ := upstreamChainServer(t, `{"object":"list","data":[{"id":"foo"},{"id":"org/bar"}]}`)

	w := getUpstream(s, "/upstream/p/foo/metrics")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/metrics", w.Body.String())

	w = getUpstream(s, "/upstream/p/org/bar/health")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "/health", w.Body.String())
}

// The llama-swap verdict survives a refetch that fails, so a peer that is
// briefly unreachable keeps the shape of its routes.
func TestPeerListingCache_LlamaSwapVerdictSurvivesFailure(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"m","meta":{"llamaswap":{"type":"model"}}}]}`)
	}))
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)
	peer := config.PeerConfig{ProxyURL: proxyURL}

	cache := peerListingCache{}
	assert.True(t, cache.isLlamaSwap("p", peer))

	fail.Store(true)
	cache.mu.Lock()
	cache.entries["p"] = peerListingEntry{llamaSwap: true} // expired
	cache.mu.Unlock()
	assert.True(t, cache.isLlamaSwap("p", peer))
	_, ok := cache.records("p", peer)
	assert.False(t, ok, "the failed fetch itself is still reported")
}

// A peer the health gate holds down is not dialled for its listing; its
// declared models fall back to synthesized records.
func TestComposePeerRecords_SkipsDownPeer(t *testing.T) {
	s, listings := upstreamChainServer(t, `{"object":"list","data":[{"id":"foo"},{"id":"extra"}]}`)
	s.peerHealth = newPeerHealthTracker(s.cfg, logmon.NewWriter(io.Discard))

	records := s.composePeerRecords("p", s.cfg.Peers["p"], 0)
	require.Len(t, records, 2)
	assert.Equal(t, int32(1), listings.Load())

	s.peerHealth.MarkDown("p", errors.New("dial tcp: no route to host"))
	s.peerListings = peerListingCache{} // forget the cached listing
	records = s.composePeerRecords("p", s.cfg.Peers["p"], 0)
	require.Len(t, records, 1, "only the declared model, synthesized")
	assert.Equal(t, "p/foo", records[0].ID)
	assert.Equal(t, map[string]any{"value": "unloaded"}, records[0].Status)
	assert.Equal(t, int32(1), listings.Load(), "no dial while down")
}
