package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// Peer records in /v1/models are forwarded from the peer's own listing so a
// federated model keeps everything its server publishes: aliases, reasoning
// and completion-cap metadata, context length, modalities, live status. The
// forwarded record differs from the remote one only in its ID and aliases
// (fully qualified with the peer prefix), its name, and a peerID marker in
// meta.llamaswap. The fetch is cached briefly and the listing falls back to
// bare records synthesized from the declared models when the peer is
// unreachable, so a down peer neither stalls nor empties the listing.

const (
	peerListingTTL     = 15 * time.Second
	peerListingTimeout = 3 * time.Second
)

type peerListingCache struct {
	mu      sync.Mutex
	entries map[string]peerListingEntry
}

type peerListingEntry struct {
	fetched time.Time
	records []modelRecord // nil when the last fetch failed
}

// records returns the peer's transformed listing, refetching when the cached
// copy (including a cached failure) is older than the TTL.
func (c *peerListingCache) records(peerID string, peer config.PeerConfig) ([]modelRecord, bool) {
	c.mu.Lock()
	entry, ok := c.entries[peerID]
	c.mu.Unlock()
	if ok && time.Since(entry.fetched) < peerListingTTL {
		return entry.records, entry.records != nil
	}

	records, err := fetchPeerListing(peerID, peer)
	if err != nil {
		records = nil
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]peerListingEntry)
	}
	c.entries[peerID] = peerListingEntry{fetched: time.Now(), records: records}
	c.mu.Unlock()
	return records, records != nil
}

func fetchPeerListing(peerID string, peer config.PeerConfig) ([]modelRecord, error) {
	// A config built without UnmarshalYAML (tests, embedding) has no parsed URL
	if peer.ProxyURL == nil {
		return nil, fmt.Errorf("peer %s has no proxy URL", peerID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), peerListingTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.ProxyURL.JoinPath("/v1/models").String(), nil)
	if err != nil {
		return nil, err
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
		req.Header.Set("x-api-key", peer.ApiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s listing: HTTP %d", peerID, resp.StatusCode)
	}

	var listing struct {
		Data []modelRecord `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, err
	}

	records := make([]modelRecord, 0, len(listing.Data))
	for _, rec := range listing.Data {
		meta, chained := annotatePeerMeta(rec.Meta, peerID)
		// A record the peer itself federated already carries a peerID; the
		// chain can reflect this server's own models back, so only first-hop
		// models are forwarded.
		if chained {
			continue
		}
		name := rec.Name
		if name == "" {
			name = rec.ID
		}
		rec.ID = config.PeerModelFQN(peerID, rec.ID)
		rec.Name = peerID + ": " + name
		rec.Meta = meta
		records = append(records, rec)
	}
	return records, nil
}

// annotatePeerMeta clones meta with meta.llamaswap.peerID set and every
// meta.llamaswap.aliases entry fully qualified, reporting whether the record
// already belonged to a further peer.
func annotatePeerMeta(meta map[string]any, peerID string) (map[string]any, bool) {
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}

	inner := make(map[string]any)
	if existing, ok := out["llamaswap"].(map[string]any); ok {
		if _, chained := existing["peerID"]; chained {
			return nil, true
		}
		for k, v := range existing {
			inner[k] = v
		}
	}

	if aliases, ok := inner["aliases"].([]any); ok {
		qualified := make([]any, len(aliases))
		for i, alias := range aliases {
			if s, ok := alias.(string); ok {
				qualified[i] = config.PeerModelFQN(peerID, s)
			} else {
				qualified[i] = alias
			}
		}
		inner["aliases"] = qualified
	}

	inner["peerID"] = peerID
	out["llamaswap"] = inner
	return out, false
}
