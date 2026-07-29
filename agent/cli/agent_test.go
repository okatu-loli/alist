package cli

import (
	"strings"
	"testing"

	"github.com/AlistGo/alist-agent/agent"
)

// ---------------------------------------------------------------------------
// --repair guard: `serve --pair` must REFUSE to re-arm the pairing acceptor
// over an already-pinned server key unless --repair is also set. This stops a
// persistent `serve --pair` service from silently re-pairing (and letting an
// attacker overwrite a good pinned key) on every restart.
// ---------------------------------------------------------------------------

func TestReverseRepairGuard(t *testing.T) {
	paired := &agent.ClientConfig{Host: "http://x:5244", AgentID: "a", ServerPubKey: "cGlubmVk"}

	// Already paired + NO --repair → REFUSE.
	if err := reverseRepairGuard(paired, false); err == nil {
		t.Fatal("already-paired agent without --repair must be refused")
	} else if !strings.Contains(err.Error(), "--repair") {
		t.Fatalf("refusal should mention --repair, got: %v", err)
	}

	// Already paired + --repair → ALLOW (the confirm gate still applies later).
	if err := reverseRepairGuard(paired, true); err != nil {
		t.Fatalf("already-paired agent WITH --repair must be allowed, got: %v", err)
	}

	// Not yet paired (empty ServerPubKey) → ALLOW regardless of --repair.
	notPaired := &agent.ClientConfig{Host: "http://x:5244", AgentID: "a"}
	if err := reverseRepairGuard(notPaired, false); err != nil {
		t.Fatalf("un-paired agent must be allowed to arm, got: %v", err)
	}

	// No config at all (never joined) → ALLOW.
	if err := reverseRepairGuard(nil, false); err != nil {
		t.Fatalf("nil config must be allowed to arm, got: %v", err)
	}
}
