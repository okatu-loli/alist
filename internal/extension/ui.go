package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveUIFile maps a request path (the *filepath after /ui/) to a real file
// inside baseDir, rejecting any path that would escape baseDir. It is the
// path-traversal gate for serving plugin bundles: clean the request, refuse
// absolute paths and "..", then confirm the resolved path is still under
// baseDir via a relative-path prefix check (belt and suspenders). An empty or
// "/" request maps to index.html. Returns the absolute file path.
func ResolveUIFile(baseDir, reqPath string) (string, error) {
	reqPath = strings.TrimPrefix(reqPath, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}
	clean := filepath.Clean("/" + reqPath) // anchor at root so ".." can't climb above it
	clean = strings.TrimPrefix(clean, "/")
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("extension: unsafe ui path %q", reqPath)
	}
	target := filepath.Join(baseDir, clean)
	rel, err := filepath.Rel(baseDir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("extension: unsafe ui path %q", reqPath)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("extension: ui path %q is a directory", reqPath)
	}
	return target, nil
}
