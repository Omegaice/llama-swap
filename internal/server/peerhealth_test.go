package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func healthTestTracker(t *testing.T, probed bool) (*peerHealthTracker, *time.Time) {
	t.Helper()
	path := ""
	if probed {
		path = "/running"
	}
	cfg := config.Config{Peers: config.PeerDictionaryConfig{
		"laptop": {Models: []string{"m"}, HealthCheckPath: path},
	}}
	tracker := newPeerHealthTracker(cfg, logmon.NewWriter(io.Discard))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	return tracker, &now
}

func TestPeerHealth_UnknownAndNilAllow(t *testing.T) {
	tracker, _ := healthTestTracker(t, true)
	assert.True(t, tracker.Allow("nobody"))
	var nilTracker *peerHealthTracker
	assert.True(t, nilTracker.Allow("laptop"))
	nilTracker.MarkDown("laptop", errors.New("x"))
	nilTracker.MarkUp("laptop")
}

func TestPeerHealth_ProbedPeerFollowsProbe(t *testing.T) {
	tracker, now := healthTestTracker(t, true)
	assert.True(t, tracker.Allow("laptop"), "nothing known yet")

	tracker.MarkDown("laptop", errors.New("dial tcp: i/o timeout"))
	assert.False(t, tracker.Allow("laptop"))

	// Backoff expiry does not reopen a probed peer; only a probe does.
	*now = now.Add(time.Hour)
	assert.False(t, tracker.Allow("laptop"))

	tracker.MarkUp("laptop")
	assert.True(t, tracker.Allow("laptop"))
	snap := tracker.Snapshot()["laptop"]
	assert.Equal(t, 0, snap.Failures)
	assert.Equal(t, *now, snap.LastSeen)
}

func TestPeerHealth_PassivePeerReopensOnBackoff(t *testing.T) {
	tracker, now := healthTestTracker(t, false)

	tracker.MarkDown("laptop", errors.New("connection refused"))
	assert.False(t, tracker.Allow("laptop"))
	assert.Equal(t, now.Add(peerHealthBackoffMin), tracker.Snapshot()["laptop"].RetryAfter)

	*now = now.Add(peerHealthBackoffMin)
	assert.True(t, tracker.Allow("laptop"), "the next request is the probe")

	// Consecutive failures double the backoff and it caps.
	tracker.MarkDown("laptop", errors.New("connection refused"))
	assert.Equal(t, now.Add(2*peerHealthBackoffMin), tracker.Snapshot()["laptop"].RetryAfter)
	for i := 0; i < 20; i++ {
		tracker.MarkDown("laptop", errors.New("connection refused"))
	}
	assert.Equal(t, now.Add(peerHealthBackoffMax), tracker.Snapshot()["laptop"].RetryAfter)

	tracker.MarkUp("laptop")
	assert.True(t, tracker.Allow("laptop"))
	assert.Equal(t, 0, tracker.Snapshot()["laptop"].Failures)
}

func TestPeerHealth_ProbeDecodesRunning(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/running":
			w.Write([]byte(`{"running":[{"model":"qwen","state":"ready"},{"model":"gemma","state":"starting"}]}`))
		case "/v1/models":
			w.Write([]byte(`{"object":"list","data":[]}`))
		case "/down":
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	proxyURL, _ := url.Parse(srv.URL)

	peer := config.PeerConfig{ProxyURL: proxyURL, ApiKey: "k", HealthCheckPath: "/running"}
	running, err := probePeer(context.Background(), peer, http.DefaultClient)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"qwen": "ready", "gemma": "starting"}, running)
	assert.Equal(t, "Bearer k", gotAuth)

	peer.HealthCheckPath = "/v1/models"
	running, err = probePeer(context.Background(), peer, http.DefaultClient)
	require.NoError(t, err)
	assert.Empty(t, running, "a liveness body carries no process states")

	peer.HealthCheckPath = "/down"
	_, err = probePeer(context.Background(), peer, http.DefaultClient)
	require.Error(t, err)

	tracker, _ := healthTestTracker(t, true)
	peer.HealthCheckPath = "/running"
	tracker.probe(context.Background(), "laptop", peer)
	assert.Equal(t, map[string]string{"qwen": "ready", "gemma": "starting"}, tracker.Running("laptop"))
	peer.HealthCheckPath = "/down"
	tracker.probe(context.Background(), "laptop", peer)
	assert.False(t, tracker.Allow("laptop"))
	assert.Nil(t, tracker.Running("laptop"), "a down peer holds nothing")
}
