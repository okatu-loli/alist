package handles

import (
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Wave 4 adversarial regressions for the /fs/search handler. These pin surface
// #4 (path injection): a caller cannot use "..", absolute-looking, or doubled
// separators in `parent` to escape their scope or launder around a recursive
// deny. Every parent is normalized by user.JoinPath (FixAndCleanPath) and gated
// by op.Authorize(ActRead) on the resulting root, and the per-directory
// FilterListing inside search.Search re-judges every child at its real path — so
// even a parent that normalizes to a broader root never spills denied files.

// TestFsSearchPathInjectionConfined drives the handler with hostile parents.
// The fixture user may read the whole mount EXCEPT a recursive-denied /sec.
func TestFsSearchPathInjectionConfined(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, mount := setupSearchFixture(t)

	// A ".." sequence must not launder the caller into the read-denied /sec:
	// mount/sec/../sec normalizes back to mount/sec, which is a 403.
	t.Run("dotdot-cannot-launder-deny", func(t *testing.T) {
		for _, p := range []string{
			mount + "/sec/../sec",
			mount + "/vis/../sec",
			mount + "/sec/./",
			mount + "/sec/deep/..", // -> mount/sec
		} {
			w := driveSearch(u, url.Values{"parent": {p}, "keyword": {"alpha"}})
			if got := respCode(t, w); got != 403 {
				t.Fatalf("parent %q must normalize into the read-denied /sec (403), got %d: %s", p, got, w.Body.String())
			}
		}
	})

	// A parent that normalizes ABOVE the mount (via "..") is still confined by
	// per-child FilterListing: climbing to "/" surfaces the readable
	// vis/alpha.txt but never the denied sec/alpha_sec.txt.
	t.Run("climb-to-root-still-confined", func(t *testing.T) {
		// mount/vis/../.. -> "/". The root "/" is traverse-authorized (the caller
		// has read below it), so this is a 200, but results must not leak sec.
		w := driveSearch(u, url.Values{"parent": {mount + "/vis/../.."}, "keyword": {"alpha"}, "per_page": {"200"}})
		if got := respCode(t, w); got != 200 {
			t.Fatalf("climbing to root should be a traverse-authorized 200, got %d: %s", got, w.Body.String())
		}
		resp := parseSearchResp(t, w)
		var sawVisible bool
		for _, it := range resp.Content {
			if strings.Contains(it.Name, "_sec") || strings.Contains(it.Parent, "/sec") {
				t.Fatalf("read-denied file leaked when parent climbed to root: %+v", it)
			}
			if it.Name == "alpha.txt" {
				sawVisible = true
			}
		}
		if !sawVisible {
			t.Fatalf("readable vis/alpha.txt must still be found, got %+v", resp.Content)
		}
	})

	// An absolute-looking path outside any granted scope is default-denied at the
	// root gate (no read on /etc/passwd -> 403), never reaching the filesystem.
	t.Run("absolute-outside-scope-403", func(t *testing.T) {
		for _, p := range []string{"/etc/passwd", "/../../etc"} {
			w := driveSearch(u, url.Values{"parent": {p}, "keyword": {"alpha"}})
			if got := respCode(t, w); got != 403 {
				t.Fatalf("parent %q outside scope must be 403, got %d: %s", p, got, w.Body.String())
			}
		}
	})

	// Degenerate separators ("//", "\\") normalize to the root "/", which this
	// caller may traverse (they read below it): a 200, but per-child
	// FilterListing still confines the result set — the denied /sec never leaks.
	t.Run("degenerate-separators-normalize-to-confined-root", func(t *testing.T) {
		for _, p := range []string{"//", `\\`} {
			w := driveSearch(u, url.Values{"parent": {p}, "keyword": {"alpha"}, "per_page": {"200"}})
			if got := respCode(t, w); got != 200 {
				t.Fatalf("parent %q normalizes to the traverse-authorized root, want 200, got %d: %s", p, got, w.Body.String())
			}
			for _, it := range parseSearchResp(t, w).Content {
				if strings.Contains(it.Name, "_sec") || strings.Contains(it.Parent, "/sec") {
					t.Fatalf("parent %q leaked a read-denied file: %+v", p, it)
				}
			}
		}
	})

	// A benign ".." round-trip that lands back inside the readable area works.
	t.Run("dotdot-roundtrip-inside-scope-ok", func(t *testing.T) {
		w := driveSearch(u, url.Values{"parent": {mount + "/sec/../vis"}, "keyword": {"alpha"}})
		if got := respCode(t, w); got != 200 {
			t.Fatalf("mount/sec/../vis normalizes to a readable dir, want 200, got %d: %s", got, w.Body.String())
		}
		resp := parseSearchResp(t, w)
		var found bool
		for _, it := range resp.Content {
			if it.Name == "alpha.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected alpha.txt from mount/sec/../vis, got %+v", resp.Content)
		}
	})
}
