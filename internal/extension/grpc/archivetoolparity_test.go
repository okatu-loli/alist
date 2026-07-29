package grpc

import (
	"context"
	"io"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchArchiveToolPlugin starts the archivetoolplugin fixture over a real
// go-plugin subprocess. Mirrors launchSearchIndexerPlugin one Kind over.
func launchArchiveToolPlugin(t *testing.T) *host.ArchiveToolClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchArchiveTool(context.Background(), exec.Command(archiveToolPluginBin), nil)
	if err != nil {
		t.Fatalf("launch archive tool plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

func drainArchiveExtract(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read extract stream: %v", err)
	}
	return b
}

// TestArchiveToolGRPCRoundTrip is Phase E wave 2's gRPC round-trip gate:
// drive Exts/Meta/List/Extract against a real go-plugin subprocess and
// confirm every field crosses the wire intact, including Extract's streaming
// shape (first chunk carries size, then data, EOF ends the stream). Mirrors
// TestSearchIndexerGRPCRoundTrip one Kind over.
func TestArchiveToolGRPCRoundTrip(t *testing.T) {
	c := launchArchiveToolPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testarchivetool" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}
	if exts := c.Exts(); len(exts) != 1 || exts[0] != "fx" {
		t.Fatalf("Exts() = %v, want [fx]", exts)
	}

	src := ext.ArchiveSource{Path: "/unused/on/this/fixture", Size: 11}

	meta, err := c.Meta(ctx, src, "")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.EntryCount != 2 || meta.TotalUncompressedSize != 11 || meta.Comment != "fixture" {
		t.Fatalf("Meta = %+v, want {fixture false 2 11}", meta)
	}

	root, err := c.List(ctx, src, "", "")
	if err != nil {
		t.Fatalf("List(root): %v", err)
	}
	if len(root) != 2 {
		t.Fatalf("List(root) = %+v, want 2 entries", root)
	}

	sub, err := c.List(ctx, src, "sub", "")
	if err != nil {
		t.Fatalf("List(sub): %v", err)
	}
	if len(sub) != 1 || sub[0].Name != "world.txt" || sub[0].Size != 6 {
		t.Fatalf("List(sub) = %+v, want one world.txt/size=6 entry", sub)
	}

	rc, size, err := c.Extract(ctx, src, "hello.txt", "")
	if err != nil {
		t.Fatalf("Extract(hello.txt): %v", err)
	}
	if size != 5 {
		t.Fatalf("Extract(hello.txt) size = %d, want 5", size)
	}
	if got := string(drainArchiveExtract(t, rc)); got != "hello" {
		t.Fatalf("Extract(hello.txt) bytes = %q, want %q", got, "hello")
	}

	rc2, size2, err := c.Extract(ctx, src, "sub/world.txt", "")
	if err != nil {
		t.Fatalf("Extract(sub/world.txt): %v", err)
	}
	if size2 != 6 {
		t.Fatalf("Extract(sub/world.txt) size = %d, want 6", size2)
	}
	if got := string(drainArchiveExtract(t, rc2)); got != "world!" {
		t.Fatalf("Extract(sub/world.txt) bytes = %q, want %q", got, "world!")
	}

	if _, _, err := c.Extract(ctx, src, "does/not/exist", ""); err == nil {
		t.Fatal("Extract(does/not/exist) = nil error, want ErrCodeObjectNotFound")
	} else if code, ok := ext.CodeOf(err); !ok || code != ext.ErrCodeObjectNotFound {
		t.Fatalf("Extract(does/not/exist) code = %v, ok=%v, want ErrCodeObjectNotFound", code, ok)
	}
}

// TestArchiveToolGRPCFindForExtThroughExtension drives the full host-side
// path this Phase E wave 2 delivers: extension.SetArchiveToolFactory ->
// extension.FindArchiveToolForExt -> a real gRPC subprocess's Meta/Extract,
// exactly the wiring server/handles/plugins.go installs in production (with
// pluginproc.Default.AcquireArchiveTool in place of this test's direct
// host.LaunchArchiveTool). Mirrors TestSearchIndexerGRPCFindByNameThroughExtension
// one Kind over.
func TestArchiveToolGRPCFindForExtThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchArchiveToolPlugin(t)

	extension.SetArchiveToolFactory(func(e *extension.Entry) (ext.ArchiveTool, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetArchiveToolFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testarchivetool",
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	tool, remote, release, ok := extension.FindArchiveToolForExt(r, ".FX")
	if !ok || tool == nil {
		t.Fatalf("FindArchiveToolForExt(.FX) = %v, %v, ok=%v; want a resolved gRPC tool", tool, remote, ok)
	}
	if !remote {
		t.Fatal("FindArchiveToolForExt should report remote=true for an ExecGRPC-resolved tool")
	}
	defer release()

	ctx := context.Background()
	meta, err := tool.Meta(ctx, ext.ArchiveSource{Path: "/unused", Size: 11}, "")
	if err != nil {
		t.Fatalf("Meta through the gRPC-resolved tool: %v", err)
	}
	if meta.EntryCount != 2 {
		t.Fatalf("Meta.EntryCount = %d, want 2", meta.EntryCount)
	}
}
