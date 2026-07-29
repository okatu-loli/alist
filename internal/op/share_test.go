package op

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// TestShareSubPathConfines is the load-bearing confinement proof: every sub is
// pinned inside the share subtree. root itself (sub == "") is allowed; a normal
// descendant resolves; and every escape shape — relative "..", a deep "../..",
// an absolute path, and a backslash-encoded "..\" — is rejected with
// errs.ShareOutOfScope.
//
// Deleting the HasPrefix gate in op.ShareSubPath turns the "../secret" /
// "../../etc/passwd" cases green-when-they-should-be-red (they would resolve to
// "/local/secret" / "/etc/passwd" and be returned), so this test fails loudly —
// that check is the single traversal gate and must never be removed.
func TestShareSubPathConfines(t *testing.T) {
	const root = "/local/pub"

	// sub == "" resolves to the share root itself (allowed).
	if got, err := ShareSubPath(root, ""); err != nil || got != root {
		t.Fatalf("root itself must be allowed: got %q err %v", got, err)
	}

	// A normal descendant resolves under the root.
	if got, err := ShareSubPath(root, "a/b.txt"); err != nil || got != "/local/pub/a/b.txt" {
		t.Fatalf("descendant must resolve: got %q err %v", got, err)
	}
	// A nested path that dips and climbs but stays inside is fine.
	if got, err := ShareSubPath(root, "a/b/../c.txt"); err != nil || got != "/local/pub/a/c.txt" {
		t.Fatalf("in-subtree climb must resolve: got %q err %v", got, err)
	}

	// Every escape shape is rejected with ShareOutOfScope.
	for name, sub := range map[string]string{
		"relative parent":  "../secret",
		"deep parent":      "../../etc/passwd",
		"climb then break": "a/../../secret",
		"absolute path":    "/local/other",
		"backslash parent": `..\secret`,
	} {
		if _, err := ShareSubPath(root, sub); !errors.Is(err, errs.ShareOutOfScope) {
			t.Fatalf("%s (%q) must be out of scope, got err %v", name, sub, err)
		}
	}
}

// TestShareSubPathExoticEscapes is the red-team hardening of the confinement
// gate: beyond the plain "../" shapes covered above, it pins the trickier
// traversal encodings against op.ShareSubPath directly (no HTTP layer), so the
// gate is proven at its narrowest point.
//
// The single most load-bearing case here is "prefix-sibling confusion":
// root="/local/pub" must NOT admit "/local/pubsecret" even though the latter has
// "/local/pub" as a STRING prefix. This is exactly why the gate checks
// HasPrefix(shareRoot+"/") (with the trailing slash) and not HasPrefix(shareRoot):
// dropping that slash would let "../pubsecret/loot" escape into a sibling whose
// name merely starts with the share-root's name. Deleting or loosening the
// trailing-slash prefix check turns these cases green-when-they-should-be-red.
func TestShareSubPathExoticEscapes(t *testing.T) {
	const root = "/local/pub"

	// --- Escapes: must all be ShareOutOfScope ---
	escapes := map[string]string{
		// Sibling directory whose NAME shares the root's prefix. Cleans to
		// "/local/pubsecret/loot" — a string-prefix match on "/local/pub" but NOT
		// a subtree member. The trailing-slash in HasPrefix is the only thing
		// standing between this and a full escape.
		"prefix-sibling":       "../pubsecret/loot",
		"prefix-sibling-dir":   "../pubsecret",
		"prefix-sibling-plain": "../publicfiles",
		// Very long "../" chain: clamps at root then lands outside.
		"long climb":  "a/../../../../../../../../etc/passwd",
		"long climb2": "../../../../../../../../../../root/.ssh/id_rsa",
		// Climb after a real segment, still breaks out.
		"climb after seg": "nested/../../outside/secret",
		// A null byte cannot smuggle an escape: the ".." still cleans and the
		// result lands outside the subtree.
		"null then climb": "x\x00/../../../etc/shadow",
		// Absolute forms (also caught up-front by the leading-"/" reject).
		"absolute sibling": "/local/pubsecret/loot",
		"backslash climb":  `..\..\outside\secret`,
	}
	for name, sub := range escapes {
		if got, err := ShareSubPath(root, sub); !errors.Is(err, errs.ShareOutOfScope) {
			t.Fatalf("%s (%q) must be ShareOutOfScope, got %q err %v", name, sub, got, err)
		}
	}

	// --- In-subtree lookalikes: NOT escapes, must resolve inside the subtree ---
	// "...." and "..." are literal directory names, not traversal, so they stay
	// confined; the resolved path is later handed to fs.Get/List which simply
	// 404s a non-existent child — it never reaches outside the share.
	inside := map[string]string{
		"quad-dot literal":   "....//secret",  // -> /local/pub/..../secret
		"triple-dot literal": ".../secret",    // -> /local/pub/.../secret
		"null in name":       "hello\x00.txt", // -> /local/pub/hello\x00.txt
		"dot-dot-x literal":  "..x/child",     // -> /local/pub/..x/child
	}
	for name, sub := range inside {
		got, err := ShareSubPath(root, sub)
		if err != nil {
			t.Fatalf("%s (%q) is a literal in-subtree name, must resolve, got err %v", name, sub, err)
		}
		if got != root && !strings.HasPrefix(got, root+"/") {
			t.Fatalf("%s (%q) resolved OUTSIDE the subtree: %q", name, sub, got)
		}
	}
}

// TestCreateAndResolveShare drives the full op path: mount a storage, create a
// share (anchoring a node on demand), and resolve it back. It also proves the
// password hash is stored one-way, the token is unguessably random and unique,
// and that a disabled or expired share collapses to a single ShareNotFound.
func TestShareCreateAndResolve(t *testing.T) {
	ctx := context.Background()
	createTestStorage(t, "/shr", t.TempDir())

	// Create a password-protected share with a future expiry.
	s, err := CreateShare(ctx, 7, "/shr/docs/report.txt", "s3cret", 24)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteShare(s.ID) })

	if len(s.Token) != shareTokenLen {
		t.Fatalf("token length: got %d want %d", len(s.Token), shareTokenLen)
	}
	if s.NodeID == 0 {
		t.Fatal("share must anchor a node")
	}
	if s.CreatorID != 7 {
		t.Fatalf("creator id: got %d", s.CreatorID)
	}
	// Password stored only as a one-way hash (never the plaintext), verifiable.
	if s.PasswordHash == "" || s.PasswordHash == "s3cret" {
		t.Fatalf("password must be hashed, got %q", s.PasswordHash)
	}
	if !utils.VerifyToken(s.PasswordHash, "s3cret") {
		t.Fatal("stored password hash must verify against the plaintext")
	}
	// Expiry set into the future.
	if s.ExpiresAt == nil || !s.ExpiresAt.After(time.Now()) {
		t.Fatalf("expiry must be set in the future: %v", s.ExpiresAt)
	}

	// Tokens are unique per share.
	s2, err := CreateShare(ctx, 7, "/shr/docs/report.txt", "", 0)
	if err != nil {
		t.Fatalf("create second share: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteShare(s2.ID) })
	if s2.Token == s.Token {
		t.Fatal("two shares must have distinct tokens")
	}
	if s2.PasswordHash != "" {
		t.Fatal("no-password share must have empty hash")
	}
	if s2.ExpiresAt != nil {
		t.Fatal("expireHours<=0 must mean no expiry")
	}

	// Resolve restores the share root virtual path from the anchored node.
	got, root, err := ResolveShare(s.Token)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ID != s.ID {
		t.Fatalf("resolve returned wrong share: %d vs %d", got.ID, s.ID)
	}
	if root != "/shr/docs/report.txt" {
		t.Fatalf("share root: got %q", root)
	}

	// An unknown token is a single, indistinguishable ShareNotFound.
	if _, _, err := ResolveShare("nope-not-a-real-token"); !errors.Is(err, errs.ShareNotFound) {
		t.Fatalf("unknown token must be ShareNotFound, got %v", err)
	}
}

// TestResolveShareExpiredAndDisabled proves the enumeration defense: an expired
// share and a disabled share both resolve to the same ShareNotFound as a missing
// token — the public layer cannot distinguish them.
func TestShareResolveExpiredAndDisabled(t *testing.T) {
	ctx := context.Background()
	createTestStorage(t, "/shrexp", t.TempDir())

	// Expired: anchor a real node, then store a share with a past expiry.
	base, err := CreateShare(ctx, 1, "/shrexp/a.txt", "", 0)
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteShare(base.ID) })

	past := time.Now().Add(-time.Hour)
	expired := &model.Share{Token: "expired-token-fixture", NodeID: base.NodeID, CreatorID: 1, ExpiresAt: &past}
	if err := db.CreateShare(expired); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteShare(expired.ID) })
	if _, _, err := ResolveShare(expired.Token); !errors.Is(err, errs.ShareNotFound) {
		t.Fatalf("expired share must be ShareNotFound, got %v", err)
	}

	disabled := &model.Share{Token: "disabled-token-fixture", NodeID: base.NodeID, CreatorID: 1, Disabled: true}
	if err := db.CreateShare(disabled); err != nil {
		t.Fatalf("create disabled: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteShare(disabled.ID) })
	if _, _, err := ResolveShare(disabled.Token); !errors.Is(err, errs.ShareNotFound) {
		t.Fatalf("disabled share must be ShareNotFound, got %v", err)
	}
}

// TestShareFollowsMove is the node-anchoring proof: a share resolves through
// NodeVirtualPath, so relocating the shared subtree (what the move/rename
// relocate handler does) makes ResolveShare report the NEW path with no edit to
// the share row — it follows because it anchors the node that moved.
func TestShareFollowsMove(t *testing.T) {
	ctx := context.Background()
	sid := createTestStorage(t, "/shrmv", t.TempDir())

	s, err := CreateShare(ctx, 3, "/shrmv/x/dir/file.txt", "", 0)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteShare(s.ID) })

	if _, root, err := ResolveShare(s.Token); err != nil || root != "/shrmv/x/dir/file.txt" {
		t.Fatalf("pre-move root: got %q err %v", root, err)
	}

	// Relocate /x/dir -> /y/dir (mirrors the relocate handler), drop path caches.
	if err := relocateNode(sid, "/x/dir", "/y/dir"); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	InvalidateAllNodePathCache()

	if _, root, err := ResolveShare(s.Token); err != nil || root != "/shrmv/y/dir/file.txt" {
		t.Fatalf("share root did not follow move: got %q err %v", root, err)
	}
}
