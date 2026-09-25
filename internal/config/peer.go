package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/tailcat"
)

type PeerDictionaryConfig map[string]PeerConfig

// PeerModelSettings decorates one of a peer's declared models with the facts
// llama-swap publishes for its own models. A peer running llama-swap reports
// all of this in its own /v1/models listing; a peer that does not (vLLM,
// SGLang, a hosted API) reports a bare id, and this is where the rest is
// declared instead.
type PeerModelSettings struct {
	// SendAs replaces the model name in the request body forwarded upstream.
	// This is useModelName for peers: it lets a declared model or alias carry
	// a name the peer itself would not recognize.
	SendAs string `yaml:"sendAs"`

	// Aliases are additional names routed to this model, addressable fully
	// qualified and, when unambiguous, bare. With filters.setParamsByID this
	// is how a peer model grows the per-alias request variants a local model
	// gets from its own setParamsByID keys.
	Aliases []string `yaml:"aliases"`

	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// Metadata rides along in /v1/models under meta.llamaswap, the same place
	// a local model's metadata block lands.
	Metadata map[string]any `yaml:"metadata"`

	// Capabilities renders the same /v1/models fields it does for a local
	// model: architecture, capabilities, supported_parameters, context_length.
	Capabilities ModelCapConfig `yaml:"capabilities"`

	// Unlisted hides the model from /v1/models while leaving it routable.
	Unlisted bool `yaml:"unlisted"`
}

type PeerConfig struct {
	Proxy      string   `yaml:"proxy"`
	ProxyURL   *url.URL `yaml:"-"`
	ApiKey     string   `yaml:"apiKey"`
	TailcatKey string   `yaml:"tailcatKey"`
	Models     []string `yaml:"models"`
	Filters    Filters  `yaml:"filters"`

	// ModelSettings decorates declared models, keyed by model ID. Every key
	// must name an entry in Models; models left out keep whatever the peer
	// itself publishes.
	ModelSettings map[string]PeerModelSettings `yaml:"modelSettings"`

	// Timeout settings for proxy connections
	Timeouts TimeoutsConfig `yaml:"timeouts"`

	tailcatBlob       string
	tailcatPrivateKey *tailcat.PrivateKey
}

// Tailcat returns the Tailcat-specific peer settings derived while validating
// a tailcat:// proxy. PrivateKey is parsed key material, retained so callers do
// not need to read the key file again. found is false for non-Tailcat peers.
func (c PeerConfig) Tailcat() (key, blob string, privateKey *tailcat.PrivateKey, found bool) {
	if c.tailcatBlob == "" {
		return "", "", nil, false
	}
	return c.TailcatKey, c.tailcatBlob, c.tailcatPrivateKey, true
}

type rawPeerConfig PeerConfig

// PeerRoutableName pairs a name a peer answers to with the declared model it
// resolves to. The two differ only for an alias.
type PeerRoutableName struct {
	Name    string
	ModelID string
}

// RoutableNames returns every name this peer answers to in declaration order:
// each declared model followed by its aliases. Duplicates are dropped, so the
// first declaration of a name wins.
func (p PeerConfig) RoutableNames() []PeerRoutableName {
	names := make([]PeerRoutableName, 0, len(p.Models))
	seen := make(map[string]struct{}, len(p.Models))

	add := func(name, modelID string) {
		if name == "" {
			return
		}
		if _, duplicate := seen[name]; duplicate {
			return
		}
		seen[name] = struct{}{}
		names = append(names, PeerRoutableName{Name: name, ModelID: modelID})
	}

	for _, modelID := range p.Models {
		add(modelID, modelID)
	}
	// Aliases follow every declared model so a name that is both a model ID
	// and some other model's alias resolves to the model.
	for _, modelID := range p.Models {
		for _, alias := range p.ModelSettings[modelID].Aliases {
			add(alias, modelID)
		}
	}
	return names
}

// UpstreamName returns the model name to place in the request body for a
// declared model: sendAs when set, the model ID otherwise.
func (p PeerConfig) UpstreamName(modelID string) string {
	if settings, ok := p.ModelSettings[modelID]; ok && settings.SendAs != "" {
		return settings.SendAs
	}
	return modelID
}

// PeerModelFQN returns the fully qualified routing name for a peer model.
// Peer and model IDs may both contain slashes, so callers must treat the
// result as an exact configured name rather than parsing it by segments.
func PeerModelFQN(peerID, modelID string) string {
	return peerID + "/" + modelID
}

// ResolvePeerModel resolves an exact fully qualified peer model name or an
// unqualified model name provided by exactly one peer. Fully qualified names
// take precedence over unqualified names that happen to contain the same text.
func (c *Config) ResolvePeerModel(search string) (peerID, modelID string, found bool) {
	for id, peer := range c.Peers {
		for _, routable := range peer.RoutableNames() {
			if PeerModelFQN(id, routable.Name) != search {
				continue
			}
			if found && (peerID != id || modelID != routable.Name) {
				return "", "", false
			}
			peerID, modelID, found = id, routable.Name, true
		}
	}
	if found {
		return peerID, modelID, true
	}

	for id, peer := range c.Peers {
		for _, routable := range peer.RoutableNames() {
			if routable.Name != search {
				continue
			}
			if found && (peerID != id || modelID != routable.Name) {
				return "", "", false
			}
			peerID, modelID, found = id, routable.Name, true
		}
	}
	return peerID, modelID, found
}

// ValidatePeerNamespace ensures every configured peer model has an
// unambiguous, reserved fully qualified name.
func ValidatePeerNamespace(c Config) error {
	type peerTarget struct {
		peerID  string
		modelID string
	}

	fqnTargets := make(map[string]peerTarget)
	peerIDs := make([]string, 0, len(c.Peers))
	for peerID := range c.Peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		for _, routable := range c.Peers[peerID].RoutableNames() {
			modelID := routable.Name

			fqn := PeerModelFQN(peerID, modelID)
			if existing, ok := fqnTargets[fqn]; ok {
				return fmt.Errorf(
					"peer model name %q is ambiguous between peers.%s model %q and peers.%s model %q",
					fqn, existing.peerID, existing.modelID, peerID, modelID,
				)
			}
			fqnTargets[fqn] = peerTarget{peerID: peerID, modelID: modelID}
		}
	}

	reserved := make([]string, 0, len(fqnTargets))
	for fqn := range fqnTargets {
		reserved = append(reserved, fqn)
	}
	sort.Strings(reserved)

	for _, fqn := range reserved {
		if _, ok := c.Models[fqn]; ok {
			return fmt.Errorf("model ID %q conflicts with fully qualified peer model name", fqn)
		}
		for modelID, model := range c.Models {
			for _, alias := range model.Aliases {
				if alias == fqn {
					return fmt.Errorf("model %s alias %q conflicts with fully qualified peer model name", modelID, fqn)
				}
			}
		}
		if _, ok := c.aliases[fqn]; ok {
			return fmt.Errorf("model alias %q conflicts with fully qualified peer model name", fqn)
		}
		if _, ok := c.Selectors[fqn]; ok {
			return fmt.Errorf("selector ID %q conflicts with fully qualified peer model name", fqn)
		}
		for profileID, profile := range c.Profiles {
			if _, ok := profile.Pins[fqn]; ok {
				return fmt.Errorf("profile %s pin %q conflicts with fully qualified peer model name", profileID, fqn)
			}
		}
	}

	return nil
}

func (c *PeerConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var probe rawPeerConfig
	if err := unmarshal(&probe); err != nil {
		return err
	}
	responseHeaderDefault := 60
	if u, err := url.Parse(probe.Proxy); err == nil && strings.EqualFold(u.Scheme, "tailcat") {
		// Tailcat peers can spend minutes loading a remote model before
		// sending response headers. Keep an explicit timeout bounded.
		responseHeaderDefault = 300
	}
	defaults := rawPeerConfig{
		Proxy:         "",
		ApiKey:        "",
		Models:        []string{},
		Filters:       Filters{},
		ModelSettings: map[string]PeerModelSettings{},

		// Mostly matches http.DefaultTransport. HTTP peers retain the 60s
		// response-header timeout from before PR #619.
		Timeouts: TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: responseHeaderDefault,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	// Validate proxy is not empty
	if defaults.Proxy == "" {
		return fmt.Errorf("proxy is required")
	}
	// Validate proxy is a valid URL and store the parsed value
	parsedURL, err := url.Parse(defaults.Proxy)
	if err != nil {
		return fmt.Errorf("invalid peer proxy URL (%s): %w", defaults.Proxy, err)
	}
	defaults.ProxyURL = parsedURL
	if err := validatePeerTailcat(&defaults); err != nil {
		return err
	}

	// Validate models is not empty
	if len(defaults.Models) == 0 {
		return fmt.Errorf("peer models can not be empty")
	}

	if err := validatePeerModelSettings(PeerConfig(defaults)); err != nil {
		return err
	}

	*c = PeerConfig(defaults)
	return nil
}

// validatePeerModelSettings rejects a modelSettings block that decorates a
// model the peer does not declare, or whose aliases collide with another name
// the same peer already answers to. Both are silent failures otherwise: the
// settings are simply never consulted, which looks like llama-swap ignoring
// the config.
func validatePeerModelSettings(peer PeerConfig) error {
	declared := make(map[string]struct{}, len(peer.Models))
	for _, modelID := range peer.Models {
		declared[modelID] = struct{}{}
	}

	settingsIDs := make([]string, 0, len(peer.ModelSettings))
	for modelID := range peer.ModelSettings {
		settingsIDs = append(settingsIDs, modelID)
	}
	sort.Strings(settingsIDs)

	for _, modelID := range settingsIDs {
		if _, ok := declared[modelID]; !ok {
			return fmt.Errorf("modelSettings key %q is not one of the peer's models", modelID)
		}
		if err := peer.ModelSettings[modelID].Capabilities.Validate(); err != nil {
			return fmt.Errorf("modelSettings %s: %w", modelID, err)
		}
	}

	// RoutableNames drops a duplicate silently, so the collision has to be
	// caught here rather than read back off the result.
	taken := make(map[string]string, len(peer.Models))
	for _, modelID := range peer.Models {
		taken[modelID] = modelID
	}
	for _, modelID := range settingsIDs {
		for _, alias := range peer.ModelSettings[modelID].Aliases {
			if alias == "" {
				return fmt.Errorf("modelSettings %s: alias can not be empty", modelID)
			}
			if owner, exists := taken[alias]; exists {
				return fmt.Errorf("modelSettings %s: alias %q is already used by model %q", modelID, alias, owner)
			}
			taken[alias] = modelID
		}
	}

	return nil
}
