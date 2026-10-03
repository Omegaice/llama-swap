package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
// records synthesized from the declared models when the peer is unreachable,
// so a down peer neither stalls nor empties the listing.
//
// A peer that does not run llama-swap publishes a bare id and nothing else.
// peers.<id>.modelSettings fills that half in from config, and takes
// precedence over the peer's own record for the fields it declares.

const (
	peerListingTTL     = 15 * time.Second
	peerListingTimeout = 3 * time.Second
)

// peerLiveRecord is one model from a peer's own listing, still carrying the
// name the peer knows it by. Qualification happens at compose time, where the
// declared model that claims this record is known.
type peerLiveRecord struct {
	upstreamID string
	aliases    []string // the peer's own alias names, unqualified
	rec        modelRecord
}

// peerTransportFunc returns the transport the router built for a peer. The
// router's dialer is the only way to reach a Tailcat peer, so every side
// channel goes through it rather than the process default.
type peerTransportFunc func(peerID string) (http.RoundTripper, bool)

// peerClient is the client a side channel uses for a peer: the router's
// transport when it knows the peer, the default otherwise (a config built
// without a router, as in tests).
func peerClient(transport peerTransportFunc, peerID string) *http.Client {
	if transport != nil {
		if rt, ok := transport(peerID); ok {
			return &http.Client{Transport: rt}
		}
	}
	return http.DefaultClient
}

type peerListingCache struct {
	mu        sync.Mutex
	entries   map[string]peerListingEntry
	transport peerTransportFunc
}

type peerListingEntry struct {
	fetched time.Time
	records []peerLiveRecord // nil when the last fetch failed
	// llamaSwap is set once the peer's records carried llama-swap's own
	// metadata, and kept across failed refetches so a peer that is briefly
	// unreachable does not change the shape of its routes.
	llamaSwap bool
}

// records returns the peer's listing, refetching when the cached copy
// (including a cached failure) is older than the TTL.
func (c *peerListingCache) records(peerID string, peer config.PeerConfig) ([]peerLiveRecord, bool) {
	entry := c.entry(peerID, peer)
	return entry.records, entry.records != nil
}

// isLlamaSwap reports whether the peer's listing identified it as another
// llama-swap. Such a peer serves a model server's own routes under
// /upstream/<model>/, not at its root.
func (c *peerListingCache) isLlamaSwap(peerID string, peer config.PeerConfig) bool {
	return c.entry(peerID, peer).llamaSwap
}

// findLive matches the front of rest against the names the peer publishes
// (ids and their aliases), longest first, so a model this config never
// declared still splits from the path that follows it. remaining starts with
// a slash, or is "/" when nothing follows the name.
func (c *peerListingCache) findLive(peerID string, peer config.PeerConfig, rest string) (name, remaining string, ok bool) {
	live, _ := c.records(peerID, peer)
	for _, entry := range live {
		for _, candidate := range append([]string{entry.upstreamID}, entry.aliases...) {
			if len(candidate) <= len(name) {
				continue
			}
			if rest == candidate || strings.HasPrefix(rest, candidate+"/") {
				name, ok = candidate, true
			}
		}
	}
	if !ok {
		return "", "", false
	}
	remaining = rest[len(name):]
	if remaining == "" {
		remaining = "/"
	}
	return name, remaining, true
}

// entry returns the peer's cache entry, refreshing it when it is older than
// the TTL.
func (c *peerListingCache) entry(peerID string, peer config.PeerConfig) peerListingEntry {
	c.mu.Lock()
	entry, ok := c.entries[peerID]
	c.mu.Unlock()
	if ok && time.Since(entry.fetched) < peerListingTTL {
		return entry
	}

	records, llamaSwap, err := fetchPeerListing(peerID, peer, peerClient(c.transport, peerID))
	if err != nil {
		records, llamaSwap = nil, entry.llamaSwap
	}
	entry = peerListingEntry{fetched: time.Now(), records: records, llamaSwap: llamaSwap}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]peerListingEntry)
	}
	c.entries[peerID] = entry
	c.mu.Unlock()
	return entry
}

// fetchPeerListing reads the peer's /v1/models. llamaSwap reports whether the
// records carried llama-swap's own metadata block, which every record a
// llama-swap lists has and a bare OpenAI-compatible server never writes.
func fetchPeerListing(peerID string, peer config.PeerConfig, client *http.Client) (records []peerLiveRecord, llamaSwap bool, err error) {
	// A config built without UnmarshalYAML (tests, embedding) has no parsed URL
	if peer.ProxyURL == nil {
		return nil, false, fmt.Errorf("peer %s has no proxy URL", peerID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), peerListingTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.ProxyURL.JoinPath("/v1/models").String(), nil)
	if err != nil {
		return nil, false, err
	}
	if peer.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+peer.ApiKey)
		req.Header.Set("x-api-key", peer.ApiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("peer %s listing: HTTP %d", peerID, resp.StatusCode)
	}

	var listing struct {
		Data []modelRecord `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, false, err
	}

	records = make([]peerLiveRecord, 0, len(listing.Data))
	for _, rec := range listing.Data {
		if _, ok := rec.Meta["llamaswap"].(map[string]any); ok {
			llamaSwap = true
		}
		meta, aliases, chained := annotatePeerMeta(rec.Meta, peerID)
		// A record the peer itself federated already carries a peerID; the
		// chain can reflect this server's own models back, so only first-hop
		// models are forwarded.
		if chained {
			continue
		}
		upstreamID := rec.ID
		if rec.Name == "" {
			rec.Name = upstreamID
		}
		rec.Meta = meta
		records = append(records, peerLiveRecord{upstreamID: upstreamID, aliases: aliases, rec: rec})
	}
	return records, llamaSwap, nil
}

// annotatePeerMeta clones meta with meta.llamaswap.peerID set, returning the
// peer's own alias names and whether the record already belonged to a further
// peer. Aliases are left unqualified; composePeerRecords qualifies whichever
// set survives.
func annotatePeerMeta(meta map[string]any, peerID string) (map[string]any, []string, bool) {
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}

	inner := make(map[string]any)
	if existing, ok := out["llamaswap"].(map[string]any); ok {
		if _, chained := existing["peerID"]; chained {
			return nil, nil, true
		}
		for k, v := range existing {
			inner[k] = v
		}
	}

	var aliases []string
	if raw, ok := inner["aliases"].([]any); ok {
		for _, alias := range raw {
			if s, ok := alias.(string); ok {
				aliases = append(aliases, s)
			}
		}
	}

	inner["peerID"] = peerID
	out["llamaswap"] = inner
	return out, aliases, false
}

// composePeerRecords builds a peer's slice of /v1/models. Every declared model
// is listed with its modelSettings applied over whatever the peer publishes
// for it, and models the peer publishes but the config never declared are
// forwarded as they arrive, so a llama-swap peer still needs no declaration to
// be discoverable.
func (s *Server) composePeerRecords(peerID string, peer config.PeerConfig, created int64) []modelRecord {
	// A peer the health gate holds down is not dialled for its listing; a
	// black-holed dial would stall the whole listing for the connect timeout.
	var live []peerLiveRecord
	if s.peerHealth.Allow(peerID) {
		live, _ = s.peerListings.records(peerID, peer)
	}

	byUpstream := make(map[string]peerLiveRecord, len(live))
	// A declared model that the peer serves as an alias of another model is
	// already advertised by that model's record; listing it again would
	// duplicate it under two ids.
	covered := make(map[string]struct{}, len(live))
	for _, entry := range live {
		byUpstream[entry.upstreamID] = entry
		covered[entry.upstreamID] = struct{}{}
		for _, alias := range entry.aliases {
			covered[alias] = struct{}{}
		}
	}

	data := make([]modelRecord, 0, len(live)+len(peer.Models))
	claimed := make(map[string]struct{}, len(peer.Models))

	for _, modelID := range peer.Models {
		settings := peer.ModelSettings[modelID]
		upstream := peer.UpstreamName(modelID)

		entry, hasLive := byUpstream[upstream]
		if hasLive {
			claimed[upstream] = struct{}{}
		} else if _, isRemoteAlias := covered[upstream]; isRemoteAlias {
			continue
		} else {
			entry = peerLiveRecord{rec: peerFallbackRecord(peerID, modelID, created)}
		}

		if settings.Unlisted {
			continue
		}

		aliases := settings.Aliases
		if len(aliases) == 0 {
			aliases = entry.aliases
		}
		rec := applyPeerModelSettings(entry.rec, settings)
		rec.Status = peerStatus(rec.Status)
		rec.ID = config.PeerModelFQN(peerID, modelID)
		rec.Name = peerID + ": " + rec.Name
		rec.Meta = withPeerAliases(rec.Meta, peerID, aliases)
		data = append(data, rec)

		if s.cfg.IncludeAliasesInList {
			for _, alias := range settings.Aliases {
				aliasRec := rec
				aliasRec.ID = config.PeerModelFQN(peerID, alias)
				aliasRec.Meta = withPeerAliases(rec.Meta, peerID, nil)
				data = append(data, aliasRec)
			}
		}
	}

	// Whatever the peer serves that the config never named.
	for _, entry := range live {
		if _, ok := claimed[entry.upstreamID]; ok {
			continue
		}
		rec := entry.rec
		rec.Status = peerStatus(rec.Status)
		rec.ID = config.PeerModelFQN(peerID, entry.upstreamID)
		rec.Name = peerID + ": " + rec.Name
		rec.Meta = withPeerAliases(rec.Meta, peerID, entry.aliases)
		data = append(data, rec)
	}

	return data
}

// peerStatus fills in the status a peer that is not llama-swap does not
// report. A model in a peer's own listing is one it is prepared to serve, and
// a client that reads status to decide what is available should not have to
// treat a missing one as a third state.
func peerStatus(status map[string]any) map[string]any {
	if len(status) > 0 {
		return status
	}
	return map[string]any{"value": "loaded"}
}

// peerFallbackRecord is the record for a declared model the peer is not
// currently publishing, either because it is unreachable or because it no
// longer serves that name.
func peerFallbackRecord(peerID, modelID string, created int64) modelRecord {
	return modelRecord{
		ID:      config.PeerModelFQN(peerID, modelID),
		Object:  "model",
		Created: created,
		OwnedBy: "llama-swap",
		Name:    modelID,
		Status:  map[string]any{"value": "unloaded"},
		Meta:    map[string]any{"llamaswap": map[string]any{"type": "peer", "peerID": peerID}},
	}
}

// applyPeerModelSettings overlays the declared settings on a record. Config
// wins field by field, the way a local model's capabilities block wins over
// discovery: a peer that publishes nothing gets a full record, and one that
// publishes a wrong field is corrected without losing the fields it got
// right.
func applyPeerModelSettings(rec modelRecord, settings config.PeerModelSettings) modelRecord {
	if settings.Name != "" {
		rec.Name = settings.Name
	}
	if settings.Description != "" {
		rec.Description = settings.Description
	}
	if !settings.Capabilities.Empty() {
		caps := settings.Capabilities.Merge(publishedCapabilities(rec))
		rec.Architecture, rec.Capabilities, rec.SupportedParameters, rec.ContextLength = renderCapabilities(caps)
		// Mirrors a local record: context_window for gateways that read that
		// name and meta.n_ctx for clients that read llama.cpp's.
		rec.ContextWindow = rec.ContextLength
		rec.Meta = withPeerContext(rec.Meta, rec.ContextLength)
	}
	if len(settings.Metadata) > 0 {
		metadata := settings.Metadata
		// Matches a local model: a capabilities block owns the top-level
		// fields it renders, so metadata may not restate them.
		if !settings.Capabilities.Empty() {
			metadata = filterCappedMetadata(metadata)
		}
		inner := peerInnerMeta(rec.Meta)
		for key, value := range metadata {
			inner[key] = value
		}
		out := make(map[string]any, len(rec.Meta)+1)
		for k, v := range rec.Meta {
			out[k] = v
		}
		out["llamaswap"] = inner
		rec.Meta = out
	}
	return rec
}

// publishedCapabilities reads the fields a peer rendered into its record back
// into the config shape, so declared capabilities can merge over them rather
// than replace them wholesale.
func publishedCapabilities(rec modelRecord) config.ModelCapConfig {
	caps := config.ModelCapConfig{
		In:       stringList(rec.Architecture["input_modalities"]),
		Out:      stringList(rec.Architecture["output_modalities"]),
		Tools:    rec.Capabilities["function_calling"] == true,
		Reranker: rec.Capabilities["reranker"] == true,
		Context:  rec.ContextLength,
	}
	if caps.Context == 0 {
		caps.Context = rec.ContextWindow
	}
	return caps
}

// stringList accepts the list as the JSON decoder delivers it or as the
// renderer builds it.
func stringList(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// withPeerContext sets meta.n_ctx to the context length, leaving meta alone
// when there is none to report.
func withPeerContext(meta map[string]any, contextLength int) map[string]any {
	if contextLength <= 0 {
		return meta
	}
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out["n_ctx"] = contextLength
	return out
}

// withPeerAliases replaces meta.llamaswap.aliases with the given names, fully
// qualified. Clients read the alias list to offer a model's request variants,
// so the names there have to be the ones this server routes.
func withPeerAliases(meta map[string]any, peerID string, aliases []string) map[string]any {
	inner := peerInnerMeta(meta)
	if len(aliases) == 0 {
		delete(inner, "aliases")
	} else {
		qualified := make([]any, 0, len(aliases))
		for _, alias := range aliases {
			qualified = append(qualified, config.PeerModelFQN(peerID, alias))
		}
		inner["aliases"] = qualified
	}

	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out["llamaswap"] = inner
	return out
}

// peerInnerMeta returns a mutable copy of meta.llamaswap.
func peerInnerMeta(meta map[string]any) map[string]any {
	inner := make(map[string]any)
	if existing, ok := meta["llamaswap"].(map[string]any); ok {
		for k, v := range existing {
			inner[k] = v
		}
	}
	return inner
}
