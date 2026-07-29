package extension

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// sourceNamePattern restricts imported source names to a safe, predictable
// charset: no embedded spaces/quotes/control characters that could confuse
// display, logging, or future shell/URL use of the name.
var sourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// PluginSource is one admin-imported plugin source: a registry index URL plus
// the ed25519 public key its packages are signed with. A package verifying
// against an imported source's key is tier 🟡 (source-verified) — attributed,
// but not first-party; install still shows a source-attribution warning.
type PluginSource struct {
	Name     string `json:"name"`
	IndexURL string `json:"index_url"`
	PubKey   string `json:"pubkey"` // ed25519 public key, hex
}

// OfficialSourceName is reserved for the built-in official source (embedded
// trust root + plugin_index_url); imported sources may not use it.
const OfficialSourceName = "official"

// Key decodes the source's ed25519 public key.
func (s PluginSource) Key() (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s.PubKey))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("extension: source %q: invalid ed25519 public key hex", s.Name)
	}
	return ed25519.PublicKey(b), nil
}

// Validate checks a source is well-formed: a non-empty, non-reserved name, an
// http(s) index URL, and a decodable key.
func (s PluginSource) Validate() error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return fmt.Errorf("extension: source name is required")
	}
	if strings.EqualFold(name, OfficialSourceName) {
		return fmt.Errorf("extension: source name %q is reserved", OfficialSourceName)
	}
	if !sourceNamePattern.MatchString(name) {
		return fmt.Errorf("extension: source name must be 1-64 chars of A-Za-z0-9._-")
	}
	u, err := url.Parse(s.IndexURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("extension: source %q: index_url must be an http(s) URL", s.Name)
	}
	_, err = s.Key()
	return err
}

// ParseSources parses the plugin_sources setting value (a JSON array of
// PluginSource). Empty input is zero sources, not an error.
func ParseSources(raw string) ([]PluginSource, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var out []PluginSource
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("extension: parse plugin_sources: %w", err)
	}
	return out, nil
}

// sourcesProvider yields the admin-imported plugin sources. Defaults to none —
// fail-closed: with no provider wired nothing can reach 🟡. The server layer
// installs a provider reading the plugin_sources setting (internal/extension
// may not import internal/setting — import cycle; same pattern as
// SetPolicyProvider).
var sourcesProvider = func() []PluginSource { return nil }

// SetSourcesProvider installs the process-wide source list provider. A nil fn
// is ignored (keeps the fail-closed default).
func SetSourcesProvider(fn func() []PluginSource) {
	if fn != nil {
		sourcesProvider = fn
	}
}

// CurrentSources reads the process-wide imported source list, live — an admin
// edit takes effect on the next install/enable/index fetch.
func CurrentSources() []PluginSource { return sourcesProvider() }
