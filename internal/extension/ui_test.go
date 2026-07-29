package extension

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUIFileServesAndBlocksTraversal(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "index.html"), []byte("<html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "assets", "app.js"), []byte("//js"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a secret sibling OUTSIDE the ui dir the traversal must not reach
	secret := filepath.Join(filepath.Dir(base), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(secret) })

	// empty path -> index.html
	got, err := ResolveUIFile(base, "")
	if err != nil || got != filepath.Join(base, "index.html") {
		t.Fatalf("empty path: got %q err %v", got, err)
	}
	// nested asset
	got, err = ResolveUIFile(base, "assets/app.js")
	if err != nil || got != filepath.Join(base, "assets", "app.js") {
		t.Fatalf("asset: got %q err %v", got, err)
	}

	for _, bad := range []string{
		"../secret.txt",
		"../../secret.txt",
		"assets/../../secret.txt",
		"/etc/passwd",
	} {
		if _, err := ResolveUIFile(base, bad); err == nil {
			t.Fatalf("traversal %q must be rejected", bad)
		}
	}
}
