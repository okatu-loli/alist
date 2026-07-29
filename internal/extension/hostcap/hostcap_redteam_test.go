package hostcap

// Red-team suite for the F0 Host capability layer. Each test is a concrete
// attack against one of the five security invariants (host.go), driven through
// the real hostcap capabilities backed by the real op/fs/Authorize core (see
// TestMain in hostcap_test.go).
//
// Two kinds of test live here:
//
//   - TestReg_* : a REGRESSION. The attack is expected to be safely denied; the
//     test asserts the denial. Permanent guards.
//   - TestVuln_* : a REAL BYPASS found during the review and SINCE FIXED. Each
//     retains its root-cause comment for history and now asserts the DENIAL the
//     fix produces (the assertion was flipped from the original characterization
//     once the hole was closed) — so a regression that reopens the hole fails here.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// ---------------------------------------------------------------------------
// VULN 1 (HIGH): host.FS().Mkdir escapes its FS grant via ".." in the name arg.
//
// hostFS.Mkdir authorizes ONLY the parent ObjRef, then does
//
//	full = FixAndCleanPath(path.Join(parentPath, name))
//
// with a plugin-controlled name and calls fs.MakeDir(full) WITHOUT
// re-authorizing full. FixAndCleanPath == stdpath.Clean, so "../" segments in
// name collapse the target OUT of the authorized subtree. fs/op perform no
// authorization of their own (fs.go: "convert mount path to actual path"), so
// the plugin writes into a mount it was never granted.
//
// Root cause: authorization is done on the parent, not on the resolved target.
// Fix: after computing `full`, run authorize(ctx, p, ActWrite, full) again (or
// reject a name containing a path separator / ".."), so the invariant "every
// op re-runs Authorize on the path it actually touches" holds for Mkdir.
// ---------------------------------------------------------------------------
func TestVuln_MkdirTraversalEscapesGrant(t *testing.T) {
	grantedRoot := mountLocal(t, "/rt-granted")
	secretRoot := mountLocal(t, "/rt-secret") // a SEPARATE mount, never granted
	_ = grantedRoot
	ctx := context.Background()

	// Plugin is granted WRITE on /rt-granted only.
	h := New(Grant{
		PluginID: "attacker",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: "/rt-granted", Write: true}},
	}, Options{})

	// Sanity: a straight write into the ungranted mount is denied.
	if _, err := h.FS().Mkdir(ctx, "", ext.ObjRef{Anchor: "/rt-secret"}, "direct"); err != ext.ErrPermissionDenied {
		t.Fatalf("precondition: direct mkdir into ungranted mount should be denied, got %v", err)
	}

	// FIXED (validateSingleSegment): a name climbing out into /rt-secret must be
	// refused and nothing created.
	_, err := h.FS().Mkdir(ctx, "", ext.ObjRef{Anchor: "/rt-granted"}, "../rt-secret/pwned")

	pwned := filepath.Join(secretRoot, "pwned")
	if _, statErr := os.Stat(pwned); statErr == nil {
		_ = os.Remove(pwned)
		t.Fatalf("REGRESSION: mkdir traversal escaped into the ungranted mount (%q created)", pwned)
	}
	if err != ext.ErrPermissionDenied {
		t.Fatalf("mkdir traversal: want ErrPermissionDenied, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// VULN 2 (HIGH): host.FS().Rename relocates a file OUT of its grant (and even
// off the storage root on disk) via ".." in the newName arg.
//
// hostFS.Rename authorizes ActRename on the SOURCE only, then calls
// fs.Rename(srcPath, name) with a plugin-controlled name. The Local driver does
// filepath.Join(filepath.Dir(realSrcPath), newName) and os.Rename, so "../"
// segments walk the file above the storage root on the real filesystem — an
// arbitrary-destination move with no destination authorization.
//
// Root cause: newName is treated as a bare leaf but is never constrained to
// one; Rename never authorizes the destination.
// Fix: reject a newName containing a path separator (a rename changes only the
// leaf), or resolve the destination and authorize ActWrite on it.
// ---------------------------------------------------------------------------
func TestVuln_RenameTraversalEscapesStorageRoot(t *testing.T) {
	root := mountLocal(t, "/rt-ren")
	ctx := context.Background()

	// A place ABOVE the storage root on disk that the plugin must not reach.
	outside := filepath.Join(filepath.Dir(root), "escaped.txt")
	_ = os.Remove(outside)

	h := New(Grant{
		PluginID: "attacker",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: "/rt-ren", Write: true}},
	}, Options{})

	// FIXED (validateSingleSegment): a newName carrying ".." must be refused, so
	// the file cannot be relocated above the storage root.
	err := h.FS().Rename(ctx, "", ext.ObjRef{Anchor: "/rt-ren/seed.txt"}, "../escaped.txt")

	if _, statErr := os.Stat(outside); statErr == nil {
		_ = os.Remove(outside)
		t.Fatalf("REGRESSION: rename traversal moved the file above the storage root (%q)", outside)
	}
	if err != ext.ErrPermissionDenied {
		t.Fatalf("rename traversal: want ErrPermissionDenied, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// VULN 3 (HIGH): host.HTTP().Do follows 3xx redirects to hosts that are NOT on
// the effective allowlist. Only the INITIAL URL's hostname is checked against
// policy ∩ Grant.Network; the stdlib http.Client (default CheckRedirect) then
// transparently follows Location headers to arbitrary hosts and returns their
// body. This is a classic SSRF allowlist bypass (reach cloud metadata,
// loopback admin APIs, internal services) from a plugin granted only one benign
// host.
//
// This test also shows the trailing-dot allowlist evasion used to obtain an
// off-allowlist hostname that still resolves to loopback deterministically:
// "127.0.0.1." != "127.0.0.1" for NetworkAllows, but dials the same address.
//
// Root cause: the allowlist is enforced once, on the request URL, not on every
// hop the transport actually dials.
// Fix: install a CheckRedirect that re-runs the policy ∩ grant check on each
// redirect target (and ideally a DialContext control that re-checks the
// resolved IP to also close DNS-rebinding).
// ---------------------------------------------------------------------------
func TestVuln_HTTPRedirectBypassesAllowlist(t *testing.T) {
	ctx := context.Background()

	// "Internal" server the plugin must NOT be able to reach.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("INTERNAL-ONLY-SECRET"))
	}))
	defer internal.Close()
	iu, _ := url.Parse(internal.URL)
	internalPort := iu.Port()

	// An off-allowlist host string that still dials loopback. "localhost"
	// resolves to 127.0.0.1 (where httptest listens) but is not string-equal to
	// the "127.0.0.1" allowlist entry, so NetworkAllows rejects it.
	offAllowlistTarget := fmt.Sprintf("http://localhost:%s/", internalPort)

	// The one benign host the plugin IS granted, which happens to redirect.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, offAllowlistTarget, http.StatusFound)
	}))
	defer front.Close()
	fu, _ := url.Parse(front.URL)

	h := New(Grant{
		PluginID: "attacker",
		HostAPIs: []GrantItem{{Name: CapNetwork}},
		Network:  []string{fu.Hostname()}, // exactly "127.0.0.1" (front's host)
	}, Options{OutboundPolicy: AllowAllOutbound})

	// Control: a DIRECT request to the off-allowlist host is correctly denied.
	if _, err := h.HTTP().Do(ctx, &ext.HTTPRequest{Method: "GET", URL: offAllowlistTarget}); err != ext.ErrPermissionDenied {
		t.Fatalf("precondition: direct request to off-allowlist host should be denied, got %v", err)
	}

	// FIXED (CheckRedirect re-checks every hop): requesting the allowed front
	// host, which 302-redirects to the off-allowlist internal host, must NOT
	// yield the internal body — the redirect is refused mid-flight.
	resp, err := h.HTTP().Do(ctx, &ext.HTTPRequest{Method: "GET", URL: front.URL})
	if err == nil {
		t.Fatalf("redirect to off-allowlist host should have been refused, got status=%d body=%q", resp.Status, string(resp.Body))
	}
	if resp != nil && strings.Contains(string(resp.Body), "INTERNAL-ONLY-SECRET") {
		t.Fatalf("REGRESSION: host.HTTP().Do followed a redirect to the off-allowlist host and returned its body")
	}
}

// ===========================================================================
// REGRESSIONS: attacks that are correctly denied today. These pin the good
// behavior so a future refactor cannot silently reopen the hole.
// ===========================================================================

// Reg: a plugin cannot forge/guess a Principal handle to act as another user.
// Handles are 24 random bytes; an unknown handle must resolve to a denial, not
// to the plugin's own bound principal or someone else's.
func TestReg_ForgedPrincipalHandleDenied(t *testing.T) {
	mountLocal(t, "/rt-prin")
	ctx := context.Background()
	h := New(Grant{
		PluginID: "p",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: "/rt-prin", Write: true}},
	}, Options{})

	for _, forged := range []ext.Principal{
		"deadbeef", "0000000000000000000000000000000000000000000000000000000000000000",
		"admin", "../admin", ext.Principal(strings.Repeat("a", 48)),
	} {
		if _, err := h.FS().List(ctx, forged, ext.ObjRef{Anchor: "/rt-prin"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
			t.Fatalf("forged principal %q: want ErrPermissionDenied, got %v", forged, err)
		}
	}
}

// Reg: invariant 1 backstop. A forged ObjRef Anchor pointing outside the grant
// is denied on every READ op — Authorize runs on the ref path, the ObjRef is
// not trusted.
func TestReg_ForgedObjRefAnchorDeniedOnReads(t *testing.T) {
	mountLocal(t, "/rt-anchor")
	secret := mountLocal(t, "/rt-anchor-secret")
	_ = secret
	ctx := context.Background()
	h := New(Grant{
		PluginID: "p",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: "/rt-anchor"}}, // read-only, granted subtree only
	}, Options{})

	outside := ext.ObjRef{Anchor: "/rt-anchor-secret/seed.txt"}
	if _, err := h.FS().Get(ctx, "", outside); err != ext.ErrPermissionDenied {
		t.Fatalf("Get forged anchor: want denied, got %v", err)
	}
	if _, err := h.FS().List(ctx, "", ext.ObjRef{Anchor: "/rt-anchor-secret"}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("List forged anchor: want denied, got %v", err)
	}
	if _, err := h.FS().Resolve(ctx, "", "/rt-anchor-secret"); err != ext.ErrPermissionDenied {
		t.Fatalf("Resolve forged anchor: want denied, got %v", err)
	}
	if _, err := h.FS().Link(ctx, "", outside, ext.LinkArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("Link forged anchor: want denied, got %v", err)
	}
	if err := h.FS().Remove(ctx, "", outside); err != ext.ErrPermissionDenied {
		t.Fatalf("Remove forged anchor: want denied, got %v", err)
	}
}

// Reg: Move/Copy authorize the DESTINATION, not just the source. A src inside
// the grant but a dst outside it must be denied.
func TestReg_MoveCopyDestinationAuthorized(t *testing.T) {
	mountLocal(t, "/rt-mv")
	mountLocal(t, "/rt-mv-secret")
	ctx := context.Background()
	h := New(Grant{
		PluginID: "p",
		HostAPIs: []GrantItem{{Name: CapFS}},
		FS:       []FSScope{{Path: "/rt-mv", Write: true}},
	}, Options{})

	src := ext.ObjRef{Anchor: "/rt-mv/seed.txt"}
	dstOutside := ext.ObjRef{Anchor: "/rt-mv-secret"}
	if err := h.FS().Move(ctx, "", src, dstOutside); err != ext.ErrPermissionDenied {
		t.Fatalf("Move to ungranted dst: want denied, got %v", err)
	}
	if err := h.FS().Copy(ctx, "", src, dstOutside); err != ext.ErrPermissionDenied {
		t.Fatalf("Copy to ungranted dst: want denied, got %v", err)
	}
}

// Reg: Store namespace isolation cannot be broken with "../" or prefix tricks
// in the key, and List(prefix) never enumerates another plugin's namespace.
func TestReg_StoreNamespaceCannotBeCrossed(t *testing.T) {
	ctx := context.Background()
	backend := NewMemKVBackend()
	victim := New(Grant{PluginID: "victim", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{KVBackend: backend})
	attacker := New(Grant{PluginID: "attacker", HostAPIs: []GrantItem{{Name: CapStore}}}, Options{KVBackend: backend})

	if err := victim.Store().KV().Set(ctx, "secret", []byte("VICTIM-DATA")); err != nil {
		t.Fatal(err)
	}
	// Try to reach the victim's namespace by mangling the key.
	for _, k := range []string{
		"../victim/secret", "..\\victim\\secret", "victim/secret",
		"secret", "/victim/secret",
	} {
		if v, ok, _ := attacker.Store().KV().Get(ctx, k); ok {
			t.Fatalf("attacker read victim data via key %q: %q", k, v)
		}
	}
	// List with an empty / broad prefix returns ONLY the attacker's own keys.
	_ = attacker.Store().KV().Set(ctx, "mine", []byte("x"))
	keys, err := attacker.Store().KV().List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if strings.Contains(k, "secret") {
			t.Fatalf("attacker List leaked victim key %q", k)
		}
	}
}

// Reg: a signed link token is bound by the core signature to its exact payload;
// tampering the ref/scope inside the token invalidates it (VerifyLink rejects),
// so a plugin cannot widen the scope or re-point the ref of a token it holds.
func TestReg_SignTokenTamperRejected(t *testing.T) {
	mnt := "/rt-sign"
	mountLocal(t, mnt)
	ctx := context.Background()
	s := New(Grant{
		PluginID: "p",
		HostAPIs: []GrantItem{{Name: CapSign}},
		FS:       []FSScope{{Path: mnt}},
	}, Options{})

	tok, err := s.Sign().SignLink(ctx, "", ext.ObjRef{StorageID: "1", Anchor: mnt}, ext.SignScopeDownload, time.Minute)
	if err != nil {
		if e, ok := err.(*ext.Error); ok && e.Code == ext.ErrCodeNotSupport {
			t.Skip("no download signer in test db; tamper leg covered by construction")
		}
		t.Fatalf("sign: %v", err)
	}
	// Tamper: keep the signature, swap the base64 payload for a wider ref.
	_, sig, _ := strings.Cut(tok, tokenSep)
	forgedPayload := encodeRefScope(ext.ObjRef{StorageID: "1", Anchor: "/etc"}, ext.SignScope("admin"))
	forged := b64Raw(forgedPayload) + tokenSep + sig
	if _, _, err := s.Sign().VerifyLink(ctx, forged); err != ext.ErrPermissionDenied {
		t.Fatalf("tampered token: want ErrPermissionDenied, got %v", err)
	}
}

// Reg: every capability method fails closed when its capability was not granted
// (invariant 5), independent of scopes present.
func TestReg_UngrantedCapabilitiesFailClosed(t *testing.T) {
	mnt := "/rt-nogrant"
	mountLocal(t, mnt)
	ctx := context.Background()
	// A grant with scopes but NO HostAPIs at all.
	h := New(Grant{
		PluginID: "p",
		Network:  []string{"*"},
		FS:       []FSScope{{Path: mnt, Write: true}},
	}, Options{OutboundPolicy: AllowAllOutbound})

	if _, err := h.FS().List(ctx, "", ext.ObjRef{Anchor: mnt}, ext.ListArgs{}); err != ext.ErrPermissionDenied {
		t.Fatalf("FS without CapFS: %v", err)
	}
	if err := h.Store().KV().Set(ctx, "k", []byte("v")); err != ext.ErrPermissionDenied {
		t.Fatalf("Store without CapStore: %v", err)
	}
	if _, err := h.Sign().SignLink(ctx, "", ext.ObjRef{Anchor: mnt}, ext.SignScopeDownload, time.Minute); err != ext.ErrPermissionDenied {
		t.Fatalf("Sign without CapSign: %v", err)
	}
	if _, err := h.HTTP().Do(ctx, &ext.HTTPRequest{Method: "GET", URL: "http://127.0.0.1/"}); err != ext.ErrPermissionDenied {
		t.Fatalf("HTTP without CapNetwork: %v", err)
	}
}

// b64Raw mirrors sign.go's base64.RawURLEncoding payload encoding.
func b64Raw(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
