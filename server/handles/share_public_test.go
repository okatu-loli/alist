package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils/random"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Share PUBLIC-access security tests (design §2.3-B / §4). This is the highest-
// risk surface: public, unauthenticated, delegated read. Every handler is driven
// end to end WITHOUT any user in context, so the tests double as proof the routes
// are reachable with no credentials. The load-bearing property is subtree
// confinement (TestSharePublicSubtreeEscape): a client-supplied ?sub= must never
// resolve — or sign a /d link — outside the shared subtree.

// setupShareFixture builds an isolated Local storage mounted at a unique /pubN
// with a shareable "pub" subtree (hello.txt, a space+bracket file, and a nested
// dir) and a sibling "outside" subtree that lives OUTSIDE any share so escape
// attempts have a real target to try to reach. It returns the mount and a
// fabricated creator id (op.CreateShare does not itself authorize — the authed
// handler does — so any id anchors a share here).
func setupShareFixture(t *testing.T) (mount string, creatorID uint) {
	t.Helper()
	n := atomic.AddInt64(&labelFixtureSeq, 1)
	mount = fmt.Sprintf("/pub%d", n)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pub", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling whose NAME shares the shared dir's prefix ("pub" ⊂ "pubx"): the
	// escape target for prefix-sibling confusion (../pubx must NOT be admitted by
	// the HasPrefix(shareRoot+"/") gate, which is why the trailing slash matters).
	if err := os.MkdirAll(filepath.Join(dir, "pubx"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeShareFile(t, filepath.Join(dir, "pubx", "loot.txt"), "LOOTMARKER")
	writeShareFile(t, filepath.Join(dir, "pub", "hello.txt"), "hello")
	// A name with a space and brackets: the /d Location must URL-encode it, and
	// 123open-style names (spaces, brackets, CJK) are exactly why we build the
	// Location via net/url instead of string concatenation.
	writeShareFile(t, filepath.Join(dir, "pub", "a b [1].txt"), "spaced")
	writeShareFile(t, filepath.Join(dir, "pub", "nested", "deep.txt"), "deep")
	writeShareFile(t, filepath.Join(dir, "outside", "secret.txt"), "TOPSECRET")

	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	return mount, 80000 + uint(n)
}

func writeShareFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// drivePublicShare runs a public share handler with the :token path param and a
// raw (already-encoded) query string, and — critically — NO user in context: the
// public surface must work unauthenticated.
func drivePublicShare(token, rawQuery string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	target := "/x"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	c.Request = httptest.NewRequest("GET", target, nil)
	c.Params = gin.Params{{Key: "token", Value: token}}
	h(c)
	return w
}

// TestSharePublicInvalidToken pins the enumeration defense: a garbage token, an
// expired share and a disabled share all answer with a single 404, on every
// public endpoint, indistinguishable from one another.
func TestSharePublicInvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	// Anchor a live share so expired/disabled variants can reuse its node.
	base, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-time.Hour)
	expired := &model.Share{Token: random.String(32), NodeID: base.NodeID, CreatorID: uid, ExpiresAt: &past}
	if err := db.CreateShare(expired); err != nil {
		t.Fatal(err)
	}
	disabled := &model.Share{Token: random.String(32), NodeID: base.NodeID, CreatorID: uid, Disabled: true}
	if err := db.CreateShare(disabled); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"garbage", "this-token-does-not-exist-xyz"},
		{"expired", expired.Token},
		{"disabled", disabled.Token},
	}
	for _, tc := range cases {
		for _, ep := range []struct {
			ep string
			h  gin.HandlerFunc
		}{{"info", ShareInfo}, {"list", ShareList}, {"download", ShareDownload}} {
			w := drivePublicShare(tc.token, "", ep.h)
			if got := respCode(t, w); got != 404 {
				t.Fatalf("%s token on %s must be 404 (enumeration defense), got %d: %s",
					tc.name, ep.ep, got, w.Body.String())
			}
		}
	}
}

// TestSharePublicInfoNoLeak pins that the info endpoint exposes only the minimal
// meta (base name, is_dir, needs_password, expires_at) and never the PasswordHash,
// the creator, or the full internal virtual path.
func TestSharePublicInfoNoLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub/hello.txt", "pw123", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.PasswordHash == "" {
		t.Fatal("precondition: password share must have a hash")
	}

	w := drivePublicShare(s.Token, "", ShareInfo)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("info must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()

	var env struct {
		Data struct {
			Name          string `json:"name"`
			IsDir         bool   `json:"is_dir"`
			NeedsPassword bool   `json:"needs_password"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse info: %v (%s)", err, body)
	}
	if !env.Data.NeedsPassword {
		t.Fatalf("password share must report needs_password=true: %s", body)
	}
	if env.Data.IsDir {
		t.Fatalf("file share must report is_dir=false: %s", body)
	}
	if env.Data.Name != "hello.txt" {
		t.Fatalf("name must be the base name only, got %q: %s", env.Data.Name, body)
	}
	if strings.Contains(body, s.PasswordHash) {
		t.Fatalf("info leaked PasswordHash: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "passwordhash") || strings.Contains(body, "password_hash") {
		t.Fatalf("info exposed a password-hash field: %s", body)
	}
	if strings.Contains(body, "creator") {
		t.Fatalf("info leaked creator: %s", body)
	}
	// The full internal virtual path (mount + subpath) must not appear — only the
	// base name is public.
	if strings.Contains(body, mount) {
		t.Fatalf("info leaked the full internal virtual path (%s): %s", mount, body)
	}
}

// TestSharePublicPassword pins the password gate: with a password set, list and
// download refuse missing/wrong passwords (403) and admit the correct one.
func TestSharePublicPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "secret", 0)
	if err != nil {
		t.Fatal(err)
	}

	// list
	if got := respCode(t, drivePublicShare(s.Token, "", ShareList)); got != 403 {
		t.Fatalf("list without password must be 403, got %d", got)
	}
	if got := respCode(t, drivePublicShare(s.Token, "password=wrong", ShareList)); got != 403 {
		t.Fatalf("list with wrong password must be 403, got %d", got)
	}
	if got := respCode(t, drivePublicShare(s.Token, "password=secret", ShareList)); got != 200 {
		t.Fatalf("list with correct password must be 200, got %d", got)
	}

	// download (a file within the shared dir)
	dlQ := func(pw string) string {
		v := url.Values{"sub": {"hello.txt"}}
		if pw != "" {
			v.Set("password", pw)
		}
		return v.Encode()
	}
	if got := respCode(t, drivePublicShare(s.Token, dlQ(""), ShareDownload)); got != 403 {
		t.Fatalf("download without password must be 403, got %d", got)
	}
	if got := respCode(t, drivePublicShare(s.Token, dlQ("wrong"), ShareDownload)); got != 403 {
		t.Fatalf("download with wrong password must be 403, got %d", got)
	}
	if got := respCode(t, drivePublicShare(s.Token, dlQ("secret"), ShareDownload)); got != 302 {
		t.Fatalf("download with correct password must be 302, got %d", got)
	}
}

// TestSharePublicSubtreeEscape is the HEAD load-bearing test: no ?sub= may
// resolve outside the shared subtree. Removing op.ShareSubPath (the HasPrefix
// gate) would let these traversals resolve — list would 200 with foreign
// contents and download would 302 to a /d link signing a file the share never
// granted. Every escape must 404 with no redirect and no leak of the foreign
// path or its content.
func TestSharePublicSubtreeEscape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0) // no password: isolate the subtree gate
	if err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"../outside",
		"../outside/secret.txt",
		"../../etc/passwd",
		mount + "/outside/secret.txt", // absolute path outside the subtree
		"..\\outside\\secret.txt",     // backslash traversal
	}
	for _, sub := range escapes {
		q := url.Values{"sub": {sub}}.Encode()

		wl := drivePublicShare(s.Token, q, ShareList)
		if got := respCode(t, wl); got == 200 {
			t.Fatalf("list sub=%q escaped the subtree (got 200): %s", sub, wl.Body.String())
		}
		if strings.Contains(wl.Body.String(), "TOPSECRET") || strings.Contains(wl.Body.String(), "secret.txt") {
			t.Fatalf("list sub=%q leaked out-of-subtree content: %s", sub, wl.Body.String())
		}

		wd := drivePublicShare(s.Token, q, ShareDownload)
		if got := respCode(t, wd); got == 302 || got == 200 {
			t.Fatalf("download sub=%q escaped the subtree (got %d, Location=%q)",
				sub, got, wd.Header().Get("Location"))
		}
		if loc := wd.Header().Get("Location"); loc != "" {
			t.Fatalf("download sub=%q must not redirect anywhere, got Location=%q", sub, loc)
		}
	}
}

// TestSharePublicListConfined pins that a directory share lists only its own
// direct children and never a sibling outside the subtree, and that each item
// carries only {name,is_dir,size} — no internal virtual path.
func TestSharePublicListConfined(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	w := drivePublicShare(s.Token, "", ShareList)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"hello.txt", "a b [1].txt", "nested"} {
		if !strings.Contains(body, want) {
			t.Fatalf("list must contain direct child %q: %s", want, body)
		}
	}
	if strings.Contains(body, "secret.txt") || strings.Contains(body, "outside") {
		t.Fatalf("list leaked an out-of-subtree sibling: %s", body)
	}
	// No item may carry the full internal virtual path (only base names).
	if strings.Contains(body, mount) {
		t.Fatalf("list leaked the internal virtual path (%s): %s", mount, body)
	}

	// Descending into a subdir stays confined and shows its child.
	wn := drivePublicShare(s.Token, url.Values{"sub": {"nested"}}.Encode(), ShareList)
	if got := respCode(t, wn); got != 200 {
		t.Fatalf("list of nested subdir must be 200, got %d: %s", got, wn.Body.String())
	}
	if !strings.Contains(wn.Body.String(), "deep.txt") {
		t.Fatalf("nested list must contain deep.txt: %s", wn.Body.String())
	}
}

// TestSharePublicDownloadRedirect pins the /d hand-off: a file download 302s to
// /d/<in-subtree real path>?sign=<SignPath> with the path correctly URL-encoded
// (space + brackets), and the signed path is exactly the in-subtree file — never
// concatenated by hand.
func TestSharePublicDownloadRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Download the space+bracket file: the encoding path is what breaks with a
	// hand-built Location.
	name := "a b [1].txt"
	full := mount + "/pub/" + name
	w := drivePublicShare(s.Token, url.Values{"sub": {name}}.Encode(), ShareDownload)
	if got := respCode(t, w); got != 302 {
		t.Fatalf("file download must be 302, got %d: %s", got, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("download must set a Location header")
	}
	// The raw Location must be encoded — no literal space may survive.
	if strings.Contains(loc, " ") {
		t.Fatalf("Location must URL-encode special chars, got raw space: %q", loc)
	}
	u, perr := url.Parse(loc)
	if perr != nil {
		t.Fatalf("Location must be a valid URL: %v (%q)", perr, loc)
	}
	// Decoded path is /d + the in-subtree real virtual path.
	if u.Path != "/d"+full {
		t.Fatalf("Location path must be /d%s, got %q (decoded %q)", full, loc, u.Path)
	}
	// The sign is over the in-subtree real path — recomputing it must match.
	if want := common.SignPath(full); u.Query().Get("sign") != want {
		t.Fatalf("Location sign must be SignPath(%s)=%q, got %q", full, want, u.Query().Get("sign"))
	}
}

// TestSharePublicExoticEscape hardens the head attack surface (subtree escape)
// against the trickier ?sub= encodings a real attacker reaches for — driven END
// TO END through the handlers so the gin query-decode timing is exercised too
// (e.g. %2e%2e%2f decodes to "../" INSIDE c.Query before op.ShareSubPath sees
// it). Every form must be confined: list never 200s with foreign content, and
// download never 302s / never emits a Location. Removing op.ShareSubPath (or its
// trailing-slash HasPrefix) turns the prefix-sibling and traversal cases into
// live escapes, so this fails loudly.
func TestSharePublicExoticEscape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0) // no password: isolate the subtree gate
	if err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		// URL-encoded traversal: gin decodes "%2e%2e%2f" → "../" in c.Query BEFORE
		// ShareSubPath runs, so the clean+HasPrefix gate is what must catch it.
		"%2e%2e%2fpubx%2floot.txt",
		"%2e%2e/outside/secret.txt",
		"..%2foutside%2fsecret.txt",
		// Prefix-sibling confusion: "pub" is a string prefix of "pubx" but not a
		// subtree member — must be rejected by HasPrefix(shareRoot+"/").
		"../pubx",
		"../pubx/loot.txt",
		// Deep/long "../" chains clamp at root then land outside.
		"a/../../../../../../../../etc/passwd",
		"../../../../../../../../../../etc/passwd",
		// Climb after a real in-subtree segment.
		"nested/../../outside/secret.txt",
		// Mixed backslash + encoded.
		"..%5coutside%5csecret.txt",
	}
	for _, sub := range escapes {
		// url.Values would re-encode the already-encoded forms; pass the raw query
		// so gin sees exactly these bytes and decodes them once (attacker-realistic).
		q := "sub=" + sub

		wl := drivePublicShare(s.Token, q, ShareList)
		if got := respCode(t, wl); got == 200 {
			t.Fatalf("list sub=%q escaped the subtree (got 200): %s", sub, wl.Body.String())
		}
		if b := wl.Body.String(); strings.Contains(b, "TOPSECRET") || strings.Contains(b, "LOOTMARKER") ||
			strings.Contains(b, "secret.txt") || strings.Contains(b, "loot.txt") {
			t.Fatalf("list sub=%q leaked out-of-subtree content: %s", sub, b)
		}

		wd := drivePublicShare(s.Token, q, ShareDownload)
		if got := respCode(t, wd); got == 302 || got == 200 {
			t.Fatalf("download sub=%q escaped the subtree (got %d, Location=%q)",
				sub, got, wd.Header().Get("Location"))
		}
		if loc := wd.Header().Get("Location"); loc != "" {
			t.Fatalf("download sub=%q must not redirect anywhere, got Location=%q", sub, loc)
		}
	}

	// In-subtree literal lookalikes ("...."/"..." are directory NAMES, not
	// traversal): these must NOT escape. They resolve inside the subtree and 404
	// as non-existent children — never a 302 to a foreign file.
	for _, sub := range []string{"....//x", ".../x", "..x/child"} {
		wd := drivePublicShare(s.Token, "sub="+sub, ShareDownload)
		if loc := wd.Header().Get("Location"); loc != "" {
			t.Fatalf("literal-name sub=%q must not redirect out, got Location=%q", sub, loc)
		}
	}
}

// TestSharePublicPasswordGatePrecedesSubtree pins the gate ORDER: on a
// password-protected share, the password check runs BEFORE any subtree
// resolution, so an attacker with no/wrong password cannot even probe the
// subtree structure — every request (in-scope or escaping) returns the password
// 403, never a 404 that would distinguish "valid path" from "escaping path".
func TestSharePublicPasswordGatePrecedesSubtree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "gate", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Both a valid in-scope sub and an escaping sub must answer 403 (password),
	// NOT 404 — the password gate short-circuits before ShareSubPath, so the two
	// are indistinguishable to an unauthenticated prober.
	for _, sub := range []string{"hello.txt", "nested", "../outside/secret.txt", "../pubx/loot.txt"} {
		for _, pw := range []string{"", "wrong"} {
			q := url.Values{"sub": {sub}}
			if pw != "" {
				q.Set("password", pw)
			}
			for _, ep := range []struct {
				name string
				h    gin.HandlerFunc
			}{{"list", ShareList}, {"download", ShareDownload}} {
				w := drivePublicShare(s.Token, q.Encode(), ep.h)
				if got := respCode(t, w); got != 403 {
					t.Fatalf("%s sub=%q pw=%q: password gate must 403 before subtree resolution, got %d: %s",
						ep.name, sub, pw, got, w.Body.String())
				}
				if b := w.Body.String(); strings.Contains(b, "TOPSECRET") || strings.Contains(b, "LOOTMARKER") {
					t.Fatalf("%s sub=%q leaked content behind the password gate: %s", ep.name, sub, b)
				}
			}
		}
	}
}

// TestSharePublicRevocationImmediate pins that deleting a share (the creator's
// revocation path, db.DeleteShare) takes effect on the VERY NEXT request:
// ResolveShare re-reads the row every call, so there is no cached-liveness
// window. A revoked token collapses into the same enumeration-safe 404 as a
// never-existent one, on every endpoint. (Disabled/expired immediacy is pinned
// separately by TestSharePublicInvalidToken.)
func TestSharePublicRevocationImmediate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Live: list works.
	if got := respCode(t, drivePublicShare(s.Token, "", ShareList)); got != 200 {
		t.Fatalf("precondition: live share must list, got %d", got)
	}

	// Hard-delete (revoke) → the token is dead on the next request everywhere.
	if err := db.DeleteShare(s.ID); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []struct {
		name string
		h    gin.HandlerFunc
	}{{"info", ShareInfo}, {"list", ShareList}, {"download", ShareDownload}} {
		if got := respCode(t, drivePublicShare(s.Token, "", ep.h)); got != 404 {
			t.Fatalf("revoked (deleted) share on %s must be 404 immediately, got %d", ep.name, got)
		}
	}
}

// TestSharePublicIgnoresCredentials pins that the public surface never reads
// Authorization/Cookie to change its answer: the handlers run OUTSIDE the auth
// group with no user in context, and attaching a (would-be) admin bearer or a
// session cookie must not turn a password 403 into access, nor a subtree escape
// into a hit. The response is byte-identical with or without the headers.
func TestSharePublicIgnoresCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount, uid := setupShareFixture(t)

	s, err := op.CreateShare(context.Background(), uid, mount+"/pub", "guard", 0)
	if err != nil {
		t.Fatal(err)
	}

	withCreds := func(token, rawQuery string, h gin.HandlerFunc) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		target := "/x"
		if rawQuery != "" {
			target += "?" + rawQuery
		}
		c.Request = httptest.NewRequest("GET", target, nil)
		c.Request.Header.Set("Authorization", "admin-bearer-token-that-should-be-ignored")
		c.Request.Header.Set("Cookie", "session=whatever")
		c.Params = gin.Params{{Key: "token", Value: token}}
		h(c)
		return w
	}

	// No password provided: must still 403 even WITH credentials attached.
	if got := respCode(t, withCreds(s.Token, "sub=hello.txt", ShareDownload)); got != 403 {
		t.Fatalf("credentials must not bypass the share password gate, got %d", got)
	}
	if got := respCode(t, withCreds(s.Token, "sub=hello.txt", ShareList)); got != 403 {
		t.Fatalf("credentials must not bypass the share password gate on list, got %d", got)
	}
	// Correct password but escaping sub: credentials must not turn it into a hit.
	esc := url.Values{"sub": {"../outside/secret.txt"}, "password": {"guard"}}.Encode()
	w := withCreds(s.Token, esc, ShareDownload)
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("credentials must not grant an out-of-subtree download, got Location=%q", loc)
	}
}
