package webdav

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
)

// TestDirFileReaddirFiltersDeniedChild is the direct, unit-level regression
// test for Finding 1 (W5 security review): dirFile.Readdir itself -- not
// merely the end-to-end PROPFIND response -- must drop a child the caller is
// denied ActRead on. It calls alistFS.OpenFile + Readdir directly, bypassing
// x/net/webdav's Handler entirely, so it cannot be accidentally satisfied by
// that vendored library's own incidental per-child re-Stat during PROPFIND's
// walkFS (see TestWebdavPropfindFiltersDeniedChild for that end-to-end
// belt-and-suspenders check): this test fails red without the
// op.FilterListing call inside Readdir, regardless of what any caller of
// this FileSystem happens to do afterward.
func TestDirFileReaddirFiltersDeniedChild(t *testing.T) {
	gateID, mount := newHandlerTestFixture(t)
	u := newTestUser(t, "pw", "")
	grantFullAccess(t, u, mount)
	r := newDavRouter(t, gateID)

	if code := doReq(r, http.MethodPut, "/dav"+mount+"/visible.txt", u.Username, "pw", []byte("v"), nil).Code; code != http.StatusCreated {
		t.Fatalf("seed visible.txt: want 201, got %d", code)
	}
	if code := doReq(r, http.MethodPut, "/dav"+mount+"/secret.txt", u.Username, "pw", []byte("s"), nil).Code; code != http.StatusCreated {
		t.Fatalf("seed secret.txt: want 201, got %d", code)
	}

	denyReadAccess(t, u, mount+"/secret.txt")

	ctx := context.WithValue(context.Background(), ctxkey.User, u)
	f, err := (alistFS{}).OpenFile(ctx, mount, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile mount dir: %v", err)
	}
	defer f.Close()

	entries, err := f.Readdir(0)
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	var sawVisible, sawSecret bool
	for _, n := range names {
		if n == "visible.txt" {
			sawVisible = true
		}
		if n == "secret.txt" {
			sawSecret = true
		}
	}
	if !sawVisible {
		t.Fatalf("Readdir must still list the permitted sibling, got %v", names)
	}
	if sawSecret {
		t.Fatalf("Readdir must not leak a child denied ActRead, got %v", names)
	}
}
