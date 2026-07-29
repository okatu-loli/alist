package extension

import (
	"crypto/ed25519"
	"strings"

	"github.com/AlistGo/alist-ext"
)

// Tier is a plugin's trust level (architecture §6.5). It is the join of two
// independent facts: who signed the package (code provenance) and whether the
// plugin is closed-source and/or paid (auditability). Tier drives the allowed
// execution modes and whether an install must pass the risk interstitial.
type Tier int

const (
	// TierThirdParty 🔴 is the least-trusted tier: unsigned, foreign-signed,
	// closed-source, or paid. It runs like every other tier (gRPC subprocess);
	// what distinguishes it is the mandatory risk interstitial at install and
	// the plugin_policy floor. This is the safe default when trust is unknown.
	TierThirdParty Tier = iota
	// TierVerified 🟡 is a package signed by an admin-imported plugin source's
	// key: attributed, but not first-party. Install shows a source-attribution
	// warning.
	TierVerified
	// TierOfficial 🟢 is a package signed by the official trust root. Installs
	// without a risk interstitial.
	TierOfficial
)

func (t Tier) String() string {
	switch t {
	case TierOfficial:
		return "official"
	case TierVerified:
		return "verified"
	case TierThirdParty:
		return "third-party"
	default:
		return "unknown"
	}
}

// ClosedOrPaid reports whether a manifest describes a closed-source and/or paid
// plugin, which forces TierThirdParty regardless of any signature. The three
// triggers (SDK Provenance) are: an explicit proprietary license, a missing
// source_url (treated as closed), or paid pricing. These are exactly the
// plugins the official/verified stores refuse to list (task 6), so the only way
// to install one is a manual URL install through the 🔴 risk path.
func ClosedOrPaid(m ext.Manifest) bool {
	p := m.Provenance
	if strings.EqualFold(strings.TrimSpace(p.License), "proprietary") {
		return true
	}
	if strings.TrimSpace(p.SourceURL) == "" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(p.Pricing), "paid") {
		return true
	}
	return false
}

// TieredVerifier verifies a package and reports the trust tier it verified at.
// It holds the official trust root plus a live provider of admin-imported
// plugin sources; a package signed by an imported source's key is 🟡, and the
// verifier reports WHICH source vouched for it (for the install warning).
type TieredVerifier struct {
	official *Verifier
	sources  func() []PluginSource // live; nil means no imported sources
}

// NewTieredVerifier builds a verifier over the official root and a source-list
// provider. Tests supply their own keys and a literal provider.
func NewTieredVerifier(official ed25519.PublicKey, sources func() []PluginSource) *TieredVerifier {
	return &TieredVerifier{official: NewVerifier(official), sources: sources}
}

// DefaultTieredVerifier is the production verifier: the embedded official root
// plus the live admin-imported source list (plugin_sources setting, wired by
// the server layer through SetSourcesProvider).
func DefaultTieredVerifier() *TieredVerifier {
	return &TieredVerifier{official: OfficialVerifier(), sources: CurrentSources}
}

// DetermineTier computes a package's trust tier and, for 🟡, the name of the
// imported source whose key verified it. Signature integrity is checked FIRST,
// before the closed/paid short-circuit: a package that presents signature
// artifacts but fails to verify against ANY trusted root (official or
// imported) is a hard rejection — SignatureError — regardless of whether the
// manifest is closed-source or paid. Only once a signature is either absent or
// successfully verified does closed/paid get to force 🔴 — a signature cannot
// buy back auditability, but it also cannot be silently ignored when it is
// present and broken. A malformed source key never grants trust (skipped). A
// cleanly unsigned package is (TierThirdParty, "", nil).
func (tv *TieredVerifier) DetermineTier(dir string, m ext.Manifest) (Tier, string, error) {
	officialOK := tv.official.Verify(dir) == nil
	sourceName := ""
	if !officialOK && tv.sources != nil {
		for _, s := range tv.sources() {
			key, err := s.Key()
			if err != nil {
				continue // malformed key never grants trust
			}
			if err := NewVerifier(key).Verify(dir); err == nil {
				sourceName = strings.TrimSpace(s.Name)
				break
			}
		}
	}
	if !officialOK && sourceName == "" && hasSignatureArtifacts(dir) {
		return TierThirdParty, "", &SignatureError{Reason: "not signed by a trusted key"}
	}
	if ClosedOrPaid(m) {
		return TierThirdParty, "", nil
	}
	if officialOK {
		return TierOfficial, "", nil
	}
	if sourceName != "" {
		return TierVerified, sourceName, nil
	}
	return TierThirdParty, "", nil
}

// ExecModesFor returns the execution modes a tier may use. Since the 2026-07-14
// plugin-parity decision every tier runs the same way — a gRPC subprocess; trust
// tiers only decide install warnings and the policy floor, not the runtime.
func ExecModesFor(t Tier) []ExecMode {
	return []ExecMode{ExecGRPC}
}

// SelectExecMode picks the execution mode for a package: every tier runs its
// native binary as a gRPC subprocess. A package that ships no runnable binary
// cannot run and is refused (ok=false).
func SelectExecMode(t Tier, hasNative bool) (mode ExecMode, ok bool) {
	if hasNative {
		return ExecGRPC, true
	}
	return ExecInProcess, false
}
