package offline_download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchRejectsSSRF asserts that every SSRF-class target (loopback, private,
// link-local incl. cloud metadata, CGNAT, unspecified) and every non-http(s)
// scheme is refused before any internal connection is made.
func TestFetchRejectsSSRF(t *testing.T) {
	bad := []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://[::1]/x",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.1/x",
		"http://192.168.1.1/x",
		"http://172.16.0.1/x",
		"http://100.64.0.1/x", // CGNAT
		"http://0.0.0.0/x",
		"file:///etc/passwd",
		"ftp://x/y",
		"gopher://x",
		"data:text/plain,x",
	}
	for _, u := range bad {
		rc, _, _, err := Fetch(context.Background(), u)
		if err == nil {
			if rc != nil {
				rc.Close()
			}
			t.Errorf("Fetch(%q) should be rejected (SSRF/scheme)", u)
		}
	}
}

// TestFetchRejectsRedirectToInternal is load-bearing: it proves the SSRF check
// runs on every hop, not just the first. The public URL is allowed but its 302
// points at loopback; the per-dial net.Dialer.Control re-check must reject it.
// Removing Control (or checking only the initial URL) turns this test red.
func TestFetchRejectsRedirectToInternal(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true // let the httptest (127.0.0.1) front server be reached
	t.Cleanup(func() { allowLoopbackForTest = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect to a loopback port that is NOT the test-allowed one; the
		// Control hook must still refuse it because the redirect target IP is
		// internal and (for this assertion) we want the hop re-checked.
		http.Redirect(w, r, "http://10.0.0.1:1/x", http.StatusFound)
	}))
	defer srv.Close()

	rc, _, _, err := Fetch(context.Background(), srv.URL)
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatal("Fetch must reject a public/allowed URL that redirects to an internal address")
	}
	// Assert the SPECIFIC SSRF rejection, not just any error. Without the
	// per-dial Control hook, dialing 10.0.0.1:1 would still fail (connection
	// error) and err != nil would pass vacuously. Requiring errSSRFRejected is
	// what makes this test prove the redirect hop was re-validated at dial time.
	if !errors.Is(err, errSSRFRejected) {
		t.Errorf("expected SSRF rejection on redirect hop, got: %v", err)
	}
}

// TestFetchHappyPath drives a real (loopback, test-allowed) server and asserts a
// streaming body, a Content-Disposition-derived name, and a correct size.
func TestFetchHappyPath(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true
	t.Cleanup(func() { allowLoopbackForTest = prev })

	want := "hello offline download"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="a.bin"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, want)
	}))
	defer srv.Close()

	rc, size, name, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch happy path failed: %v", err)
	}
	defer rc.Close()

	if name != "a.bin" {
		t.Errorf("name = %q, want %q", name, "a.bin")
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if size != int64(len(want)) {
		t.Errorf("size = %d, want %d", size, len(want))
	}
}

// TestFetchSanitizesFilename ensures a malicious Content-Disposition filename is
// reduced to a bare filename (no traversal, no separators).
func TestFetchSanitizesFilename(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true
	t.Cleanup(func() { allowLoopbackForTest = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../etc/passwd"`)
		io.WriteString(w, "x")
	}))
	defer srv.Close()

	rc, _, name, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	defer rc.Close()

	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		t.Errorf("name %q must be a bare filename (no separators/traversal)", name)
	}
	if name != "passwd" {
		t.Errorf("name = %q, want %q", name, "passwd")
	}
}
