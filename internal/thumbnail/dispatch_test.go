package thumbnail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// fakeThumbnailer is a fully-controllable ext.Thumbnailer double for pinning
// the HOST-side dispatch layer added in Phase E wave 3: Generate must route
// to whichever Thumbnailer claims the source's extension (not always the
// built-in image renderer), and must enforce maxThumbOutputBytes on the
// dispatched tool's OWN returned bytes exactly like it would the built-in
// renderer's -- a Thumbnailer is never trusted to have already bounded its
// own output. Mirrors internal/archive/dispatch_test.go's fakeArchiveTool.
type fakeThumbnailer struct {
	exts     []string
	renderFn func(maxDim int) ([]byte, string, error)
}

var _ ext.Thumbnailer = fakeThumbnailer{}

func (f fakeThumbnailer) Manifest() ext.Manifest {
	return ext.Manifest{ID: "builtin.thumbnail.testfake"}
}
func (f fakeThumbnailer) Init(ctx context.Context, h ext.Host) error { return nil }
func (f fakeThumbnailer) Shutdown(ctx context.Context) error         { return nil }
func (f fakeThumbnailer) Exts() []string                             { return f.exts }
func (f fakeThumbnailer) Render(ctx context.Context, src ext.ThumbSource, maxDim int) ([]byte, string, error) {
	return f.renderFn(maxDim)
}

var fakeThumbFixtureSeq int64

// setupFakeThumbFixture mounts an isolated Local storage containing a single
// file named data.<fileExt> (arbitrary bytes -- the fake tool never actually
// looks at them) and registers tool under a fresh, unique
// builtin.thumbnail.testfake entry claiming fileExt. Returns a caller scoped
// to ActRead and the file's full virtual path. flags.DataDir is NOT touched
// here -- callers that reach the cache-write path must redirect it
// themselves (mirrors setupThumbFixture).
func setupFakeThumbFixture(t *testing.T, fileExt string, tool ext.Thumbnailer) (reader *model.User, path string) {
	t.Helper()
	n := atomic.AddInt64(&fakeThumbFixtureSeq, 1)
	mount := fmt.Sprintf("/fakethumb%d", n)

	rootDir := t.TempDir()
	fileName := "data." + fileExt
	if err := os.WriteFile(filepath.Join(rootDir, fileName), []byte("irrelevant-source-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + rootDir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	entryID := fmt.Sprintf("builtin.thumbnail.testfake%d", n)
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    entryID,
		Kinds: []ext.Kind{ext.KindThumbnailer},
	}); err != nil {
		t.Fatal(err)
	}
	extension.RegisterTrustedThumbnailerInstance(entryID, tool)
	t.Cleanup(func() { extension.RegisterTrustedThumbnailerInstance(entryID, nil) })

	reader = &model.User{
		ID:       82000 + uint(n),
		Username: fmt.Sprintf("fakethumbreader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      mount,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}
	return reader, mount + "/" + fileName
}

// TestGenerateDispatchesToRegisteredThumbnailerHit: a source extension no
// built-in image format claims ("fk") but a registered fake Thumbnailer
// does -- Generate must dispatch to it and return exactly its bytes,
// including writing them to the on-disk cache.
func TestGenerateDispatchesToRegisteredThumbnailerHit(t *testing.T) {
	oldDataDir := flags.DataDir
	flags.DataDir = t.TempDir()
	t.Cleanup(func() { flags.DataDir = oldDataDir })

	want := []byte("fake-thumbnailer-bytes")
	tool := fakeThumbnailer{
		exts: []string{"fk"},
		renderFn: func(maxDim int) ([]byte, string, error) {
			return want, "image/x-fake", nil
		},
	}
	reader, path := setupFakeThumbFixture(t, "fk", tool)

	data, cachedPath, err := Generate(context.Background(), reader, path, 100)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("Generate returned %q, want the dispatched tool's bytes %q", data, want)
	}
	if cachedPath == "" {
		t.Fatal("Generate returned empty cachedPath")
	}
	if got, err := os.ReadFile(cachedPath); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("cache file = %q, %v; want %q written to disk", got, err, want)
	}
}

// TestGenerateRejectsOversizeThumbnailerOutput: a dispatched Thumbnailer that
// hands back more than maxThumbOutputBytes is refused with
// ErrRenderedThumbnailTooLarge -- the host-side output cap applies to a
// third-party tool's bytes exactly as it would the built-in renderer's,
// since a Thumbnailer is never trusted to have already bounded its own
// response.
func TestGenerateRejectsOversizeThumbnailerOutput(t *testing.T) {
	oldDataDir := flags.DataDir
	flags.DataDir = t.TempDir()
	t.Cleanup(func() { flags.DataDir = oldDataDir })

	oldCap := maxThumbOutputBytes
	maxThumbOutputBytes = 8
	t.Cleanup(func() { maxThumbOutputBytes = oldCap })

	tool := fakeThumbnailer{
		exts: []string{"fk2"},
		renderFn: func(maxDim int) ([]byte, string, error) {
			return bytes.Repeat([]byte{0xAB}, 1000), "image/x-fake", nil
		},
	}
	reader, path := setupFakeThumbFixture(t, "fk2", tool)

	_, _, err := Generate(context.Background(), reader, path, 100)
	if !errors.Is(err, ErrRenderedThumbnailTooLarge) {
		t.Fatalf("Generate error = %v, want ErrRenderedThumbnailTooLarge", err)
	}
}

// TestGenerateUnclaimedExtensionFallsBackToNotThumbnailable: no registered
// Thumbnailer (built-in or fake) claims "fk3" -- Generate must report the
// exact same ErrNotImageThumbnailable a plain unsupported extension always
// did, rather than e.g. falling back to the built-in image renderer for a
// format it never claimed.
func TestGenerateUnclaimedExtensionFallsBackToNotThumbnailable(t *testing.T) {
	oldDataDir := flags.DataDir
	flags.DataDir = t.TempDir()
	t.Cleanup(func() { flags.DataDir = oldDataDir })

	n := atomic.AddInt64(&fakeThumbFixtureSeq, 1)
	mount := fmt.Sprintf("/fakethumbmiss%d", n)
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "data.fk3"), []byte("irrelevant"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + rootDir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	reader := &model.User{
		ID:       83000 + uint(n),
		Username: fmt.Sprintf("fakethumbmissreader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      mount,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}

	_, _, err = Generate(context.Background(), reader, mount+"/data.fk3", 100)
	if !errors.Is(err, ErrNotImageThumbnailable) {
		t.Fatalf("Generate(unclaimed ext) error = %v, want ErrNotImageThumbnailable", err)
	}
}
