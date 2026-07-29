package handles

import "testing"

// The username-cleanliness guard on federated auto-provisioning (defense-in-
// depth for the confusable/junk-account surface). Authorization is role-based,
// so these were never escalation — this just refuses to mint junk accounts.
func TestValidateProvisionedUsername(t *testing.T) {
	bad := []string{"", " ", "  ", "admin ", " admin", "ad min", "a\tb", "a\nb", "x\x00y", "​x"}
	for _, n := range bad {
		if err := validateProvisionedUsername(n); err == nil {
			t.Errorf("expected reject for %q, got nil", n)
		}
	}
	good := []string{"alice", "bob.smith", "user_1", "Admin", "用户", "a-b"}
	for _, n := range good {
		if err := validateProvisionedUsername(n); err != nil {
			t.Errorf("expected accept for %q, got %v", n, err)
		}
	}
}
