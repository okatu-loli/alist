package thememarket

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
)

func init() {
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	// Every test below hits FetchCatalog/DownloadTheme against an
	// httptest.NewServer, i.e. 127.0.0.1 — a loopback address the production
	// marketplaceHTTPClient refuses (M1 SSRF hardening). This flips that
	// block off for this package's own test binary only; see
	// AllowInternalHostsForTests's doc comment (httpclient.go) for why this
	// is safe (no production code path can reach it).
	AllowInternalHostsForTests(true)
}

func TestSourceValidate(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)

	cases := []struct {
		name  string
		src   Source
		isErr bool
	}{
		{"valid, no pubkey (untrusted-capable)", Source{Name: "myrepo", IndexURL: "https://example.com/index.json"}, false},
		{"valid, with pubkey", Source{Name: "myrepo", IndexURL: "https://example.com/index.json", PubKey: pubHex}, false},
		{"empty name", Source{Name: "", IndexURL: "https://example.com/index.json"}, true},
		{"bad charset name", Source{Name: "my repo!", IndexURL: "https://example.com/index.json"}, true},
		{"non-http url", Source{Name: "myrepo", IndexURL: "ftp://example.com/index.json"}, true},
		{"malformed pubkey", Source{Name: "myrepo", IndexURL: "https://example.com/index.json", PubKey: "zz"}, true},
	}
	for _, c := range cases {
		err := c.src.Validate()
		if c.isErr && err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
		if !c.isErr && err != nil {
			t.Errorf("%s: expected no error, got %v", c.name, err)
		}
	}
}

func TestSourceKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := Source{Name: "x", PubKey: hex.EncodeToString(pub)}
	key, ok, err := s.Key()
	if err != nil || !ok || !key.Equal(pub) {
		t.Fatalf("expected the decoded key back, got key=%v ok=%v err=%v", key, ok, err)
	}

	blank := Source{Name: "x"}
	key, ok, err = blank.Key()
	if err != nil || ok || key != nil {
		t.Fatalf("blank pubkey must be (nil, false, nil), got key=%v ok=%v err=%v", key, ok, err)
	}

	bad := Source{Name: "x", PubKey: "not-hex"}
	if _, _, err := bad.Key(); err == nil {
		t.Fatal("malformed pubkey must error")
	}
}

func TestParseSources(t *testing.T) {
	if s, err := ParseSources(""); err != nil || s != nil {
		t.Fatalf("empty: want (nil, nil), got (%v, %v)", s, err)
	}
	if s, err := ParseSources("[]"); err != nil || s != nil {
		t.Fatalf("[]: want (nil, nil), got (%v, %v)", s, err)
	}
	if _, err := ParseSources("{not json"); err == nil {
		t.Fatal("bad JSON must error")
	}
	s, err := ParseSources(`[{"name":"a","index_url":"https://x/index.json"}]`)
	if err != nil || len(s) != 1 || s[0].Name != "a" {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestFetchCatalogRoundTripsMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"nord","name":"Nord","url":"https://x/nord.json","sha256":"abc","mode":"dark"},
			{"id":"day","name":"Day","url":"https://x/day.json","sha256":"def","mode":"light"},
			{"id":"both","name":"Both","url":"https://x/both.json","sha256":"ghi"}
		]`))
	}))
	defer srv.Close()

	entries, err := FetchCatalog(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Mode != "dark" || entries[1].Mode != "light" || entries[2].Mode != "" {
		t.Fatalf("got %+v", entries)
	}
}

func TestFetchCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"nord","name":"Nord","url":"https://x/nord.json","sha256":"abc"}]`))
	}))
	defer srv.Close()

	entries, err := FetchCatalog(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "nord" {
		t.Fatalf("got %+v", entries)
	}
}

func TestFetchCatalogRejectsBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	if _, err := FetchCatalog(context.Background(), srv.URL); err == nil {
		t.Fatal("bad JSON catalog must be rejected")
	}
}

func TestFetchCatalogRejectsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := FetchCatalog(context.Background(), srv.URL); err == nil {
		t.Fatal("non-200 status must be rejected")
	}
}

func TestFetchCatalogRejectsEmptyURL(t *testing.T) {
	if _, err := FetchCatalog(context.Background(), ""); err == nil {
		t.Fatal("empty index url must be rejected")
	}
}

// signedEntry builds a CatalogEntry whose Signature verifies against priv for
// the given content's sha256 digest.
func signedEntry(t *testing.T, priv ed25519.PrivateKey, id string, content []byte) CatalogEntry {
	t.Helper()
	return signedEntryV(t, priv, id, "1.0.0", "https://x/"+id+".json", content)
}

// signedEntryV builds a fully-specified, correctly-signed CatalogEntry: the
// signature covers the CANONICAL id|version|sha256|url tuple (I3 fix), not
// merely the sha256 digest, so tests exercising tamper/replay must construct
// entries this way (or deliberately vary one field, per
// TestVerifyEntry_SignatureDoesNotCoverXXX below) to get a genuinely valid
// signature.
func signedEntryV(t *testing.T, priv ed25519.PrivateKey, id, version, url string, content []byte) CatalogEntry {
	t.Helper()
	sum := sha256.Sum256(content)
	entry := CatalogEntry{ID: id, Version: version, URL: url, SHA256: hex.EncodeToString(sum[:])}
	sig := ed25519.Sign(priv, canonicalEntrySigningInput(entry))
	entry.Signature = hex.EncodeToString(sig)
	return entry
}

func TestVerifyEntry_NoPubkeySourceIsUntrustedNotRejected(t *testing.T) {
	src := Source{Name: "nokey"}
	entry := CatalogEntry{ID: "x", SHA256: strings.Repeat("00", 32)}
	tier, err := VerifyEntry(src, entry)
	if err != nil {
		t.Fatalf("no-pubkey source must never be rejected, got: %v", err)
	}
	if tier != TierUntrusted {
		t.Fatalf("tier = %q, want %q", tier, TierUntrusted)
	}
}

func TestVerifyEntry_ValidSignatureIsVerified(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Name: "signed", PubKey: hex.EncodeToString(pub)}
	entry := signedEntry(t, priv, "nord", []byte(`{"id":"nord"}`))
	tier, err := VerifyEntry(src, entry)
	if err != nil {
		t.Fatalf("valid signature must verify, got: %v", err)
	}
	if tier != TierVerified {
		t.Fatalf("tier = %q, want %q", tier, TierVerified)
	}
}

func TestVerifyEntry_MissingSignatureWithPubkeySourceIsRejected(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Name: "signed", PubKey: hex.EncodeToString(pub)}
	entry := CatalogEntry{ID: "x", SHA256: strings.Repeat("00", 32)}
	if _, err := VerifyEntry(src, entry); err == nil {
		t.Fatal("a source with a pubkey must reject an entry with no signature")
	}
}

func TestVerifyEntry_WrongSignatureIsRejected(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Name: "signed", PubKey: hex.EncodeToString(pub)}
	// Signed by a DIFFERENT key than the source's pubkey.
	entry := signedEntry(t, otherPriv, "nord", []byte(`{"id":"nord"}`))
	if _, err := VerifyEntry(src, entry); err == nil {
		t.Fatal("a signature from the wrong key must be rejected")
	}
}

// TestVerifyEntry_SignatureDoesNotTransferToADifferentEntry is the I3
// red-team regression: a (digest, signature) pair harvested from one
// legitimately-signed entry must NOT verify once pasted onto an entry that
// differs in id, version, or url — even though the sha256 digest and
// signature bytes themselves are copied verbatim from a genuinely-signed
// entry. Before the canonical-signing-input fix, the signature covered ONLY
// the sha256 digest, so an attacker (or a compromised/careless source) could
// take one signed (digest, signature) pair and republish it under a
// completely different id/name/url and it would still read "verified".
func TestVerifyEntry_SignatureDoesNotTransferToADifferentEntry(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Name: "signed", PubKey: hex.EncodeToString(pub)}
	genuine := signedEntryV(t, priv, "nord", "1.0.0", "https://good.example/nord.json", []byte(`{"id":"nord"}`))

	// Sanity: the genuine entry, unmodified, verifies.
	if _, err := VerifyEntry(src, genuine); err != nil {
		t.Fatalf("genuine entry must verify, got: %v", err)
	}

	cases := []struct {
		name  string
		build func() CatalogEntry
	}{
		{"different id, same digest+signature", func() CatalogEntry {
			e := genuine
			e.ID = "evil-twin"
			return e
		}},
		{"different version, same digest+signature", func() CatalogEntry {
			e := genuine
			e.Version = "9.9.9"
			return e
		}},
		{"different url, same digest+signature", func() CatalogEntry {
			e := genuine
			e.URL = "https://evil.example/nord.json"
			return e
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := VerifyEntry(src, c.build()); err == nil {
				t.Fatalf("%s: signature must NOT verify once a signed field is changed without re-signing", c.name)
			}
		})
	}
}

func TestVerifyEntry_MalformedSourcePubkeyErrors(t *testing.T) {
	src := Source{Name: "bad", PubKey: "not-hex"}
	entry := CatalogEntry{ID: "x", SHA256: strings.Repeat("00", 32)}
	if _, err := VerifyEntry(src, entry); err == nil {
		t.Fatal("malformed source pubkey must error")
	}
}

func TestDownloadTheme(t *testing.T) {
	content := []byte(`{"id":"nord","name":{"en":"Nord"},"light":{"primary":"#5e81ac"}}`)
	sum := sha256.Sum256(content)
	shaHex := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	got, err := DownloadTheme(context.Background(), srv.URL, shaHex)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("got %q, want %q", got, content)
	}
}

func TestDownloadThemeRejectsSHA256Mismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"nord"}`))
	}))
	defer srv.Close()

	wrongSum := sha256.Sum256([]byte("something else entirely"))
	_, err := DownloadTheme(context.Background(), srv.URL, hex.EncodeToString(wrongSum[:]))
	if err == nil {
		t.Fatal("sha256 mismatch must be rejected")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error should mention sha256 mismatch, got: %v", err)
	}
}

func TestDownloadThemeRejectsOversized(t *testing.T) {
	huge := strings.Repeat("a", MaxThemeDownloadBytes+1)
	sum := sha256.Sum256([]byte(huge))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	_, err := DownloadTheme(context.Background(), srv.URL, hex.EncodeToString(sum[:]))
	if err == nil {
		t.Fatal("oversized download must be rejected")
	}
}

func TestDownloadThemeRejectsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	sum := sha256.Sum256([]byte("x"))
	if _, err := DownloadTheme(context.Background(), srv.URL, hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("non-200 status must be rejected")
	}
}

func TestDownloadThemeRejectsNonHTTPURL(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	if _, err := DownloadTheme(context.Background(), "file:///etc/passwd", hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("non-http(s) url must be rejected")
	}
}

func TestValidateDescriptor(t *testing.T) {
	id, hasJS, err := ValidateDescriptor([]byte(`{"id":"nord","light":{}}`))
	if err != nil || id != "nord" || hasJS {
		t.Fatalf("got id=%q hasJS=%v err=%v", id, hasJS, err)
	}

	id, hasJS, err = ValidateDescriptor([]byte(`{"id":"evil","js":"alert(1)"}`))
	if err != nil || id != "evil" || !hasJS {
		t.Fatalf("got id=%q hasJS=%v err=%v (want hasJS=true, no error — js is ALLOWED to be stored)", id, hasJS, err)
	}

	if _, _, err := ValidateDescriptor([]byte(`{not json`)); err == nil {
		t.Fatal("bad JSON must be rejected")
	}
	if _, _, err := ValidateDescriptor([]byte(`{"name":{"en":"x"}}`)); err == nil {
		t.Fatal("missing id must be rejected")
	}
	if _, _, err := ValidateDescriptor([]byte(`{"id":""}`)); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if _, _, err := ValidateDescriptor([]byte(`{"id":123}`)); err == nil {
		t.Fatal("non-string id must be rejected")
	}
}

func TestValidateDescriptorEmptyJSStillFalse(t *testing.T) {
	_, hasJS, err := ValidateDescriptor([]byte(`{"id":"x","js":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if hasJS {
		t.Fatal("an empty-string js field must report hasJS=false")
	}
}

func TestParseInstalledThemes(t *testing.T) {
	if l, err := ParseInstalledThemes(""); err != nil || l != nil {
		t.Fatalf("empty: want (nil, nil), got (%v, %v)", l, err)
	}
	if l, err := ParseInstalledThemes("[]"); err != nil || l != nil {
		t.Fatalf("[]: want (nil, nil), got (%v, %v)", l, err)
	}
	if _, err := ParseInstalledThemes("{not json"); err == nil {
		t.Fatal("bad JSON must error")
	}
	raw := `[{"id":"nord","source":"myrepo","entry_id":"nord","tier":"verified","theme":{"id":"nord"}}]`
	l, err := ParseInstalledThemes(raw)
	if err != nil || len(l) != 1 || l[0].ID != "nord" {
		t.Fatalf("got %+v, %v", l, err)
	}
	var themeObj map[string]json.RawMessage
	if err := json.Unmarshal(l[0].Theme, &themeObj); err != nil {
		t.Fatalf("Theme should round-trip as raw JSON: %v", err)
	}
}

// --- M1 hardening: SSRF guard + per-op timeout ------------------------------

// TestFetchCatalogBlocksInternalHosts and TestDownloadThemeBlocksInternalHosts
// pin the SSRF guard by temporarily flipping AllowInternalHostsForTests off —
// every OTHER test in this file leaves it on (see this file's init) so they
// can use httptest.NewServer at all; these two specifically verify what
// happens in the PRODUCTION posture (internal hosts blocked).
func TestFetchCatalogBlocksInternalHosts(t *testing.T) {
	AllowInternalHostsForTests(false)
	defer AllowInternalHostsForTests(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	_, err := FetchCatalog(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("fetching a catalog from a loopback address must be refused in the production posture")
	}
	var netErr *NetworkError
	if !isNetworkError(err, &netErr) {
		t.Errorf("expected a *NetworkError, got: %T %v", err, err)
	}
}

func TestDownloadThemeBlocksInternalHosts(t *testing.T) {
	AllowInternalHostsForTests(false)
	defer AllowInternalHostsForTests(true)

	content := []byte(`{"id":"nord"}`)
	sum := sha256.Sum256(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	_, err := DownloadTheme(context.Background(), srv.URL, hex.EncodeToString(sum[:]))
	if err == nil {
		t.Fatal("downloading from a loopback address must be refused in the production posture")
	}
}

// isNetworkError is errors.As without importing "errors" just for this one
// helper — kept trivial since *NetworkError implements Unwrap and this is the
// only place that needs the type assertion chain.
func isNetworkError(err error, target **NetworkError) bool {
	for err != nil {
		if ne, ok := err.(*NetworkError); ok {
			*target = ne
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestFetchCatalogWrapsTransportFailureAsNetworkError(t *testing.T) {
	// A closed local port: connection refused, a transport-level failure.
	_, err := FetchCatalog(context.Background(), "http://127.0.0.1:1/index.json")
	if err == nil {
		t.Fatal("expected an error")
	}
	var netErr *NetworkError
	if !isNetworkError(err, &netErr) {
		t.Errorf("expected a *NetworkError, got: %T %v", err, err)
	}
}

func TestFetchCatalogNonOKStatusIsNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := FetchCatalog(context.Background(), srv.URL)
	var netErr *NetworkError
	if !isNetworkError(err, &netErr) {
		t.Errorf("expected a *NetworkError, got: %T %v", err, err)
	}
}

func TestFetchCatalogBadJSONIsNotANetworkError(t *testing.T) {
	// A parse failure is a VALIDATION error (safe to show verbatim), distinct
	// from a transport failure — must NOT be wrapped as *NetworkError.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	_, err := FetchCatalog(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected an error")
	}
	var netErr *NetworkError
	if isNetworkError(err, &netErr) {
		t.Error("a JSON parse failure must not be classified as a NetworkError")
	}
}

func TestMarketplaceRequestTimeoutIsBounded(t *testing.T) {
	// Pins M1's requirement ("15-30s") without actually sleeping that long in
	// the test suite.
	if marketplaceRequestTimeout < 15_000_000_000 || marketplaceRequestTimeout > 30_000_000_000 {
		t.Fatalf("marketplaceRequestTimeout = %v, want between 15s and 30s", marketplaceRequestTimeout)
	}
}
