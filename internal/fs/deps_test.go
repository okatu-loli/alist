package fs

// Go has no runtime dependency-graph query usable from a test, so this pins
// the fs-layer decoupling by scanning this package's own source files for
// forbidden import paths (server/gin/drivers-s3) rather than walking the
// transitive import graph.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFsHasNoUpwardDependencies(t *testing.T) {
	forbidden := []string{
		"github.com/alist-org/alist/v3/server",
		"github.com/gin-gonic/gin",
		"github.com/alist-org/alist/v3/drivers/s3",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s imports forbidden %s — fs layer must stay decoupled", f, bad)
			}
		}
	}
}
