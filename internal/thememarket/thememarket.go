// Package thememarket implements the theme-marketplace catalog/trust/download
// machinery for theming spec §J5 (最后一波: 主题市场). It deliberately mirrors
// internal/extension's plugin-source shape (sources.go) rather than inventing
// a parallel trust system — a theme-marketplace source is the same
// {name, index_url, pubkey} shape an admin already manages for plugins — but
// stays a SEPARATE, standalone package: a theme is a declarative JSON
// descriptor with no code-execution risk beyond the SAME per-theme js-trust
// gate every other theme source (site_theme, user themes, plugin
// contributed themes) already goes through, so the heavier plugin trust
// machinery (multi-file manifest verification, ClosedOrPaid gating, gRPC
// execution modes) simply does not apply here. There is also no built-in
// "official" trust root for themes (unlike plugins' embedded key) — every
// theme source is admin-imported, so every theme is at best TierVerified
// (signed by an imported source's key) or TierUntrusted (no key configured).
package thememarket

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// sourceNamePattern mirrors extension.sources.go's identical restriction:
// no embedded spaces/quotes/control characters that could confuse display,
// logging, or future shell/URL use of the name.
var sourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Source is one admin-imported theme-marketplace source. Unlike
// extension.PluginSource, PubKey is OPTIONAL: a source with none has no
// signing capability, so every theme it serves installs at TierUntrusted —
// still installable (see VerifyEntry), just not attributable to a signer.
type Source struct {
	Name     string `json:"name"`
	IndexURL string `json:"index_url"`
	PubKey   string `json:"pubkey,omitempty"` // ed25519 public key, hex; optional
}

// Key decodes the source's ed25519 public key. ok is false (with a nil
// error) when PubKey is blank — that is a valid, deliberate configuration
// (an unsigned/untrusted source), not a malformed one. A non-blank PubKey
// that fails to decode to exactly an ed25519 key IS an error.
func (s Source) Key() (key ed25519.PublicKey, ok bool, err error) {
	p := strings.TrimSpace(s.PubKey)
	if p == "" {
		return nil, false, nil
	}
	b, err := hex.DecodeString(p)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, false, fmt.Errorf("thememarket: source %q: invalid ed25519 public key hex", s.Name)
	}
	return ed25519.PublicKey(b), true, nil
}

// Validate checks a source is well-formed: a non-empty name of the
// restricted charset, an http(s) index URL, and — only if PubKey is
// non-blank — a decodable key.
func (s Source) Validate() error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return fmt.Errorf("thememarket: source name is required")
	}
	if !sourceNamePattern.MatchString(name) {
		return fmt.Errorf("thememarket: source name must be 1-64 chars of A-Za-z0-9._-")
	}
	u, err := url.Parse(s.IndexURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("thememarket: source %q: index_url must be an http(s) URL", s.Name)
	}
	_, _, err = s.Key()
	return err
}

// ParseSources parses the theme_sources setting value (a JSON array of
// Source). Empty input is zero sources, not an error.
func ParseSources(raw string) ([]Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var out []Source
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("thememarket: parse theme_sources: %w", err)
	}
	return out, nil
}

// CatalogEntry is one theme-marketplace listing (an index.json element).
type CatalogEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Author  string `json:"author,omitempty"`
	Version string `json:"version,omitempty"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Preview string `json:"preview,omitempty"`
	HasJS   bool   `json:"has_js,omitempty"`
	// Mode is the catalog author's own declaration of which colour scheme(s)
	// the theme targets: "light", "dark", or "" (unspecified/both). Purely
	// descriptive metadata for the marketplace UI's mode filter — never
	// verified or enforced server-side (a theme's ACTUAL light/dark palettes
	// are whatever its light/dark token maps say once installed; this field
	// only drives browse-time filtering before that content is even
	// downloaded).
	Mode string `json:"mode,omitempty"`
	// Signature is a hex ed25519 signature over this entry's CANONICAL
	// signing input — id|version|sha256|url (see canonicalEntrySigningInput)
	// — NOT merely the sha256 digest alone. Binding id/version/url into the
	// signed material (opus final-review I3) is what stops a (digest,
	// signature) pair harvested from one legitimately-signed entry from being
	// pasted onto a DIFFERENT entry (different id/name/url, same bytes) and
	// still reading "verified" — any of those fields changing invalidates the
	// signature. It does NOT by itself stop a genuinely-signed OLD version
	// from being re-served to force a downgrade; that is closed separately by
	// the install-time monotonic-version guard (see
	// server/handles/theme_market.go). Present only when the owning source
	// signs its catalog; see VerifyEntry.
	Signature string `json:"signature,omitempty"`
}

// canonicalEntrySigningInput builds the exact bytes a catalog source must
// sign (and VerifyEntry re-derives to check): the pipe-joined tuple
// id|version|sha256|url. Binding all four into one signed string means a
// signature cannot be replayed onto an entry that differs in ANY of them —
// see CatalogEntry.Signature's doc comment for the attack this closes.
func canonicalEntrySigningInput(entry CatalogEntry) []byte {
	return []byte(entry.ID + "|" + entry.Version + "|" + entry.SHA256 + "|" + entry.URL)
}

// maxCatalogBytes caps a fetched index.json — it is a listing of metadata,
// not payloads, so even a large marketplace catalog should stay well under
// this; a source serving more is almost certainly malformed or hostile.
const maxCatalogBytes = 4 << 20 // 4 MiB

// FetchCatalog fetches and parses indexURL as a JSON array of CatalogEntry.
// Bounded by marketplaceRequestTimeout (M1) regardless of ctx's own deadline,
// and routed through marketplaceHTTPClient (SSRF-hardened — see
// httpclient.go), not the shared internal/net.HttpClient(). Any transport-
// level failure (dial/DNS/TLS/timeout/non-2xx/redirect-refused) is wrapped in
// *NetworkError so callers can show a generic message and log the detailed
// one server-side (M6) — a parse/shape failure is returned as a plain error
// (safe to show verbatim; it never embeds network internals).
func FetchCatalog(ctx context.Context, indexURL string) ([]CatalogEntry, error) {
	if strings.TrimSpace(indexURL) == "" {
		return nil, fmt.Errorf("thememarket: index url is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, marketplaceRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return nil, &NetworkError{Op: "fetch catalog", Err: err}
	}
	resp, err := marketplaceHTTPClient.Do(req)
	if err != nil {
		return nil, &NetworkError{Op: "fetch catalog", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &NetworkError{Op: "fetch catalog", Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes))
	if err != nil {
		return nil, &NetworkError{Op: "fetch catalog", Err: err}
	}
	var entries []CatalogEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("thememarket: parse catalog: %w", err)
	}
	return entries, nil
}

// Trust tiers for a marketplace theme. There is no TierOfficial (see package
// doc) — every theme is admin-sourced, at best TierVerified.
const (
	TierVerified  = "verified"
	TierUntrusted = "untrusted"
)

// decodeSHA256Hex decodes a hex sha256 digest, requiring exactly 32 bytes.
func decodeSHA256Hex(hexDigest string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(hexDigest))
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("thememarket: invalid sha256 hex")
	}
	return b, nil
}

// VerifyEntry determines the trust tier for one catalog entry from `source`.
//
//   - A source with no PubKey configured always yields (TierUntrusted, nil):
//     there is no key to check a signature against, so nothing can be
//     attributed — but nothing is REJECTED either. This mirrors the plugin
//     third-party tier: still installable, just labelled untrusted.
//   - A source WITH a PubKey requires every entry it serves to carry a
//     Signature that verifies against that key (over the entry's own sha256
//     digest bytes). A missing or non-verifying signature is a hard
//     rejection (err != nil) — a source that claims a trust root but ships
//     an entry that doesn't verify against it is a stronger red flag than
//     "no signature at all" (tampering, key mismatch, or a misconfigured
//     source), so it fails closed rather than silently downgrading to
//     untrusted.
func VerifyEntry(source Source, entry CatalogEntry) (tier string, err error) {
	key, hasKey, err := source.Key()
	if err != nil {
		return "", err
	}
	if !hasKey {
		return TierUntrusted, nil
	}
	sig := strings.TrimSpace(entry.Signature)
	if sig == "" {
		return "", fmt.Errorf("thememarket: source %q requires a signature but entry %q carries none", source.Name, entry.ID)
	}
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return "", fmt.Errorf("thememarket: entry %q: invalid signature hex", entry.ID)
	}
	// Also require SHA256 to at least be well-formed hex (DownloadTheme
	// re-verifies the real digest against downloaded bytes independently;
	// this is just guarding the signing-input construction below).
	if _, err := decodeSHA256Hex(entry.SHA256); err != nil {
		return "", err
	}
	if !ed25519.Verify(key, canonicalEntrySigningInput(entry), sigBytes) {
		return "", fmt.Errorf("thememarket: entry %q: signature does not verify against source %q's key", entry.ID, source.Name)
	}
	return TierVerified, nil
}

// MaxThemeDownloadBytes caps a single downloaded theme payload — generous
// above a realistic hand-authored theme (tokens + a restricted css block +
// branding/notification, the same content shape as op.SiteThemeMaxBytes),
// but far below what would let a hostile/misconfigured source source exhaust
// server memory/disk on install.
const MaxThemeDownloadBytes = 512 << 10 // 512 KiB

// DownloadTheme fetches rawURL, enforces MaxThemeDownloadBytes, and verifies
// the downloaded bytes hash to expectedSHA256 EXACTLY — a mismatch is a hard
// rejection regardless of trust tier (this check runs even for an untrusted
// source; sha256 pinning at least guarantees the admin gets the exact bytes
// the catalog described, not a moved/tampered target). Returns the raw bytes
// for the caller to further parse; does not itself confirm the content is a
// valid theme descriptor — see ValidateDescriptor.
func DownloadTheme(ctx context.Context, rawURL string, expectedSHA256 string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("thememarket: theme url must be an http(s) URL")
	}
	want, err := decodeSHA256Hex(expectedSHA256)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, marketplaceRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &NetworkError{Op: "download theme", Err: err}
	}
	resp, err := marketplaceHTTPClient.Do(req)
	if err != nil {
		return nil, &NetworkError{Op: "download theme", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &NetworkError{Op: "download theme", Err: fmt.Errorf("unexpected status %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxThemeDownloadBytes+1))
	if err != nil {
		return nil, &NetworkError{Op: "download theme", Err: err}
	}
	if len(data) > MaxThemeDownloadBytes {
		return nil, fmt.Errorf("thememarket: theme exceeds the %d byte download limit", MaxThemeDownloadBytes)
	}
	sum := sha256.Sum256(data)
	if subtle.ConstantTimeCompare(sum[:], want) != 1 {
		return nil, fmt.Errorf("thememarket: sha256 mismatch: expected %s, got %s", expectedSHA256, hex.EncodeToString(sum[:]))
	}
	return data, nil
}

// ValidateDescriptor does a SHALLOW check that data looks like a theme
// descriptor: valid JSON object with a non-empty string "id" field. Deep
// validation (token colours, css sanitization, js gating) happens on the
// FRONTEND at load time via the same prepareTheme gate every other theme
// source runs through (site_theme, user themes, plugin themes) — the backend
// intentionally does not reimplement that gate in Go; it only guards what a
// server-stored JSON blob needs guarded here (parses, has an id). hasJS
// reports whether the descriptor carries a non-blank top-level "js" field —
// callers use this for bookkeeping (InstalledTheme.HasJS), not as a security
// gate (storing js is allowed for marketplace themes; see conf.MarketThemes).
func ValidateDescriptor(data []byte) (id string, hasJS bool, err error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return "", false, fmt.Errorf("thememarket: theme payload is not a valid JSON object: %w", err)
	}
	idRaw, ok := obj["id"]
	if !ok {
		return "", false, fmt.Errorf(`thememarket: theme payload is missing "id"`)
	}
	if err := json.Unmarshal(idRaw, &id); err != nil || strings.TrimSpace(id) == "" {
		return "", false, fmt.Errorf(`thememarket: theme payload's "id" must be a non-empty string`)
	}
	if jsRaw, ok := obj["js"]; ok {
		var js string
		if err := json.Unmarshal(jsRaw, &js); err == nil && strings.TrimSpace(js) != "" {
			hasJS = true
		}
	}
	return id, hasJS, nil
}

// InstalledTheme is one theme installed from the marketplace, as stored in
// the conf.MarketThemes setting (a JSON array of these). Theme carries the
// raw, verbatim downloaded descriptor bytes — never re-serialized — so the
// frontend's prepareTheme gate sees exactly what was hash-verified at
// install time.
type InstalledTheme struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	EntryID     string          `json:"entry_id"`
	Name        string          `json:"name,omitempty"`
	Author      string          `json:"author,omitempty"`
	Version     string          `json:"version,omitempty"`
	Tier        string          `json:"tier"`
	HasJS       bool            `json:"has_js"`
	InstalledAt int64           `json:"installed_at"`
	Theme       json.RawMessage `json:"theme"`
}

// ParseInstalledThemes parses the market_installed_themes setting value (a
// JSON array of InstalledTheme). Empty input is zero themes, not an error.
func ParseInstalledThemes(raw string) ([]InstalledTheme, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var out []InstalledTheme
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("thememarket: parse market_installed_themes: %w", err)
	}
	return out, nil
}
