package extension

import "strings"

// Policy is the admin's installed-plugin trust floor (setting plugin_policy). It
// is enforced at BOTH install and enable (load) time — defense in depth against
// a state file that was written under a looser policy and a policy later
// tightened, or a hand-edited state file.
type Policy int

const (
	// PolicyOfficialOnly (the default, most restrictive) admits only 🟢 official
	// packages. 🟡 verified and 🔴 third-party are rejected.
	PolicyOfficialOnly Policy = iota
	// PolicyVerified admits 🟢 official and 🟡 verified; 🔴 third-party rejected.
	PolicyVerified
	// PolicyAny admits every tier. Every plugin runs the same way (gRPC
	// subprocess); "any" widens what may install — the risk interstitial and
	// the admin's own judgement are the remaining guard.
	PolicyAny
)

// Setting values for plugin_policy.
const (
	PolicyOfficialOnlyValue = "official-only"
	PolicyVerifiedValue     = "verified"
	PolicyAnyValue          = "any"
)

// ParsePolicy maps a setting string to a Policy, defaulting to the most
// restrictive (official-only) for empty/unknown input — an unreadable policy
// must never silently open the gate.
func ParsePolicy(s string) Policy {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case PolicyVerifiedValue:
		return PolicyVerified
	case PolicyAnyValue:
		return PolicyAny
	default:
		return PolicyOfficialOnly
	}
}

func (p Policy) String() string {
	switch p {
	case PolicyVerified:
		return PolicyVerifiedValue
	case PolicyAny:
		return PolicyAnyValue
	default:
		return PolicyOfficialOnlyValue
	}
}

// Allows reports whether a package of tier t may install/load under this policy.
func (p Policy) Allows(t Tier) bool {
	switch p {
	case PolicyAny:
		return true
	case PolicyVerified:
		return t >= TierVerified
	default: // official-only
		return t >= TierOfficial
	}
}

// policyProvider yields the current admin policy. It defaults to the most
// restrictive so a Manager built before the setting layer wires a real provider
// still fails closed. The server layer (which may import internal/setting,
// whereas internal/extension may not — that would be an import cycle) installs a
// provider reading the plugin_policy setting via SetPolicyProvider.
var policyProvider = func() Policy { return PolicyOfficialOnly }

// SetPolicyProvider installs the process-wide policy source. A nil fn is ignored
// (keeps the fail-closed default).
func SetPolicyProvider(fn func() Policy) {
	if fn != nil {
		policyProvider = fn
	}
}

// CurrentPolicy reads the process-wide policy. Manager uses this indirection so
// a policy change (admin edits the setting) takes effect on the next
// install/enable without rebuilding the Manager.
func CurrentPolicy() Policy { return policyProvider() }
