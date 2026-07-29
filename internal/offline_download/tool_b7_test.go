package offline_download

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/extension"
)

// B7 per-request tool picker: AvailableToolNames is what the frontend dialog
// lists. SimpleHttp is always present and first (the always-available base
// direct path); every registered+enabled tool follows in sorted order; a
// registered-but-disabled tool is excluded so the list matches exactly what
// AddOfflineDownload accepts.

func TestAvailableToolNamesSimpleHttpAlwaysFirst(t *testing.T) {
	resetBase(t)
	got := AvailableToolNames()
	if len(got) == 0 || got[0] != "SimpleHttp" {
		t.Fatalf("AvailableToolNames() = %v, want SimpleHttp first", got)
	}
}

func TestAvailableToolNamesIncludesEnabledExcludesDisabled(t *testing.T) {
	resetBase(t)
	// Two enabled tools registered out of alphabetical order; one gets disabled.
	_ = regTool(t, "zeta")
	_ = regTool(t, "alpha")
	t.Cleanup(func() { resetBase(t) })

	got := AvailableToolNames()
	if got[0] != "SimpleHttp" {
		t.Fatalf("AvailableToolNames()[0] = %q, want SimpleHttp", got[0])
	}
	if !contains(got, "alpha") || !contains(got, "zeta") {
		t.Fatalf("AvailableToolNames() = %v, want both enabled tools present", got)
	}
	// Sorted after SimpleHttp: alpha before zeta.
	if idx(got, "alpha") > idx(got, "zeta") {
		t.Fatalf("AvailableToolNames() = %v, want alpha before zeta (sorted)", got)
	}

	// Disable one owning extension: it drops out of the list.
	if err := extension.DefaultRegistry.Disable("builtin.offline_download.zeta"); err != nil {
		t.Fatalf("disable zeta: %v", err)
	}
	got = AvailableToolNames()
	if contains(got, "zeta") {
		t.Fatalf("AvailableToolNames() = %v, must exclude the disabled tool zeta", got)
	}
	if !contains(got, "alpha") {
		t.Fatalf("AvailableToolNames() = %v, must still contain the enabled tool alpha", got)
	}
	if got[0] != "SimpleHttp" {
		t.Fatalf("AvailableToolNames()[0] = %q, want SimpleHttp even after a disable", got[0])
	}
}

func contains(s []string, v string) bool {
	return idx(s, v) >= 0
}

func idx(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
