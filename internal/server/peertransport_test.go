package server

import (
	"context"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// countingTransport wraps the default transport and counts what passes
// through, standing in for the dialer only the router owns.
func countingTransport(seen *atomic.Int32) peerTransportFunc {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen.Add(1)
		return http.DefaultTransport.RoundTrip(req)
	})
	return func(peerID string) (http.RoundTripper, bool) {
		return rt, peerID == "p"
	}
}

// The listing fetch and the health probe reach the peer through the transport
// the router built for it, never the process default.
func TestPeerSideChannels_UsePeerTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	proxyURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	peer := config.PeerConfig{ProxyURL: proxyURL, Models: []string{"m"}, HealthCheckPath: "/v1/models"}

	var seen atomic.Int32
	cache := peerListingCache{transport: countingTransport(&seen)}
	records, ok := cache.records("p", peer)
	require.True(t, ok)
	require.Len(t, records, 1)
	assert.Equal(t, int32(1), seen.Load(), "listing fetched through the peer transport")

	cfg := config.Config{Peers: config.PeerDictionaryConfig{"p": peer}}
	tracker := newPeerHealthTracker(cfg, logmon.NewWriter(io.Discard))
	tracker.transport = countingTransport(&seen)
	tracker.probe(context.Background(), "p", peer)
	assert.True(t, tracker.Allow("p"))
	assert.Equal(t, int32(2), seen.Load(), "probe went through the peer transport")

	// An unknown peer falls back to the default client rather than failing.
	tracker.transport = func(string) (http.RoundTripper, bool) { return nil, false }
	tracker.probe(context.Background(), "p", peer)
	assert.True(t, tracker.Allow("p"))
	assert.Equal(t, int32(2), seen.Load())
}

// The router hands out the transport of every configured peer and nothing
// for a name it does not know.
func TestPeerRouter_Transport(t *testing.T) {
	proxyURL, _ := url.Parse("http://127.0.0.1:1")
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"p": {Proxy: proxyURL.String(), ProxyURL: proxyURL, Models: []string{"m"}},
	}}
	peer, err := router.NewPeer(cfg, logmon.NewWriter(io.Discard))
	require.NoError(t, err)
	rt, ok := peer.Transport("p")
	assert.True(t, ok)
	assert.NotNil(t, rt)
	_, ok = peer.Transport("q")
	assert.False(t, ok)
}
