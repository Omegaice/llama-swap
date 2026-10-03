package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// Peer health is what lets a selector route around a node that is gone and
// lets a request for a down peer fail at once instead of waiting out a dial.
//
// A peer with a healthCheckPath is probed on its interval for as long as the
// server runs; the probe result is the peer's state, so a node that closed
// its lid drops out of every pool within one interval and comes back the same
// way. A peer without one is passive: a transport error on a forwarded
// request marks it down, and the mark expires on a backoff so the next
// request through is the probe. Any response from the peer, error status or
// not, marks it up: the node answered.
//
// A probe of /running also records the peer's process states, which is what
// a placement that prefers a node already holding the model reads.

const (
	peerHealthProbeTimeout = 3 * time.Second
	peerHealthBackoffMin   = 10 * time.Second
	peerHealthBackoffMax   = 5 * time.Minute
)

type peerHealthState struct {
	Healthy    bool      `json:"healthy"`
	Probed     bool      `json:"probed"`
	LastSeen   time.Time `json:"lastSeen,omitzero"`
	LastError  string    `json:"lastError,omitempty"`
	Failures   int       `json:"failures"`
	RetryAfter time.Time `json:"retryAfter,omitzero"`
	// Running is the peer's own /running listing, model id to state, when the
	// probe path returns one.
	Running map[string]string `json:"running,omitempty"`
}

type peerHealthTracker struct {
	mu        sync.Mutex
	peers     map[string]*peerHealthState
	logger    *logmon.Monitor
	now       func() time.Time
	transport peerTransportFunc
}

func newPeerHealthTracker(cfg config.Config, logger *logmon.Monitor) *peerHealthTracker {
	t := &peerHealthTracker{
		peers:  make(map[string]*peerHealthState, len(cfg.Peers)),
		logger: logger,
		now:    time.Now,
	}
	for peerID, peer := range cfg.Peers {
		// An unprobed peer starts healthy: nothing is known against it.
		t.peers[peerID] = &peerHealthState{Healthy: true, Probed: peer.HealthCheckPath != ""}
	}
	return t
}

// Allow reports whether a request may be forwarded to the peer. A probed peer
// is allowed exactly when its last probe succeeded. A passive peer that is
// marked down is allowed again once its backoff has expired, so that request
// becomes the probe.
func (t *peerHealthTracker) Allow(peerID string) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.peers[peerID]
	if !ok || state.Healthy {
		return true
	}
	return !state.Probed && !t.now().Before(state.RetryAfter)
}

// RetryAfter returns when a down peer will next be allowed; the zero time for
// a healthy peer. A probed peer reports its next probe as unknown (zero).
func (t *peerHealthTracker) retryAfter(peerID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if state, ok := t.peers[peerID]; ok && !state.Healthy {
		return state.RetryAfter
	}
	return time.Time{}
}

// MarkUp records that the peer answered.
func (t *peerHealthTracker) MarkUp(peerID string) {
	t.markUp(peerID, nil)
}

func (t *peerHealthTracker) markUp(peerID string, running map[string]string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.peers[peerID]
	if !ok {
		return
	}
	if !state.Healthy {
		t.logger.Infof("peer %s: up", peerID)
	}
	state.Healthy = true
	state.LastSeen = t.now()
	state.LastError = ""
	state.Failures = 0
	state.RetryAfter = time.Time{}
	if running != nil {
		state.Running = running
	}
}

// MarkDown records that the peer could not be reached. The backoff doubles
// per consecutive failure from peerHealthBackoffMin to peerHealthBackoffMax.
func (t *peerHealthTracker) MarkDown(peerID string, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.peers[peerID]
	if !ok {
		return
	}
	state.Failures++
	backoff := peerHealthBackoffMin << (state.Failures - 1)
	if state.Failures > 16 || backoff > peerHealthBackoffMax {
		backoff = peerHealthBackoffMax
	}
	if state.Healthy {
		t.logger.Warnf("peer %s: down: %v", peerID, err)
	}
	state.Healthy = false
	state.LastError = err.Error()
	state.RetryAfter = t.now().Add(backoff)
	state.Running = nil
}

// Running returns the peer's last reported process states, or nil.
func (t *peerHealthTracker) Running(peerID string) map[string]string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.peers[peerID]
	if !ok || state.Running == nil {
		return nil
	}
	out := make(map[string]string, len(state.Running))
	for k, v := range state.Running {
		out[k] = v
	}
	return out
}

// Snapshot returns a copy of every peer's state, keyed by peer id.
func (t *peerHealthTracker) Snapshot() map[string]peerHealthState {
	out := make(map[string]peerHealthState)
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, state := range t.peers {
		copied := *state
		copied.Running = nil
		if state.Running != nil {
			copied.Running = make(map[string]string, len(state.Running))
			for k, v := range state.Running {
				copied.Running[k] = v
			}
		}
		out[id] = copied
	}
	return out
}

// startPolling launches one prober per peer that declares a healthCheckPath.
// Probers stop when ctx is cancelled.
func (t *peerHealthTracker) startPolling(ctx context.Context, cfg config.Config) {
	if t == nil {
		return
	}
	peerIDs := make([]string, 0, len(cfg.Peers))
	for peerID := range cfg.Peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)
	for _, peerID := range peerIDs {
		peer := cfg.Peers[peerID]
		if peer.HealthCheckPath == "" || peer.ProxyURL == nil {
			continue
		}
		go t.poll(ctx, peerID, peer)
	}
}

func (t *peerHealthTracker) poll(ctx context.Context, peerID string, peer config.PeerConfig) {
	interval := time.Duration(peer.HealthCheckInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		t.probe(ctx, peerID, peer)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *peerHealthTracker) probe(ctx context.Context, peerID string, peer config.PeerConfig) {
	running, err := probePeer(ctx, peer, peerClient(t.transport, peerID))
	if err != nil {
		t.MarkDown(peerID, err)
		return
	}
	t.markUp(peerID, running)
}

// probePeer fetches the peer's health path. A body shaped like /running
// ({"running": [{"model", "state"}, ...]}) is decoded into process states;
// any other 2xx body is accepted as a bare liveness answer.
func probePeer(ctx context.Context, peer config.PeerConfig, client *http.Client) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, peerHealthProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.ProxyURL.JoinPath(peer.HealthCheckPath).String(), nil)
	if err != nil {
		return nil, err
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
		req.Header.Set("x-api-key", peer.ApiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("health check %s: HTTP %d", peer.HealthCheckPath, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var listing struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if json.Unmarshal(body, &listing) != nil || listing.Running == nil {
		return map[string]string{}, nil
	}
	running := make(map[string]string, len(listing.Running))
	for _, entry := range listing.Running {
		running[entry.Model] = entry.State
	}
	return running, nil
}
