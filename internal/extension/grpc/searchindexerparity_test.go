package grpc

import (
	"context"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchSearchIndexerPlugin starts the searchindexerplugin fixture over a
// real go-plugin subprocess. Mirrors launchOfflineDownloadToolPlugin one Kind
// over.
func launchSearchIndexerPlugin(t *testing.T) *host.SearchIndexerClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchSearchIndexer(context.Background(), exec.Command(searchIndexerPluginBin), nil)
	if err != nil {
		t.Fatalf("launch search indexer plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestSearchIndexerGRPCRoundTrip is Phase E ring 1b's gRPC round-trip gate:
// drive Config/Index/BatchIndex/Get/Search/Del/Clear against a real
// go-plugin subprocess and confirm every field crosses the wire intact,
// including through the fixture's real in-memory node table (not just an
// echo-via-error trick, since Search/Get have to actually reflect what was
// indexed). Mirrors TestOfflineDownloadToolGRPCRoundTrip one Kind over.
func TestSearchIndexerGRPCRoundTrip(t *testing.T) {
	c := launchSearchIndexerPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testsearchindexer" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	cfg := c.Config()
	if cfg.Name != "echoindexer" || !cfg.AutoUpdate {
		t.Fatalf("Config() = %+v, want {echoindexer true}", cfg)
	}

	if err := c.Index(ctx, ext.SearchNode{Parent: "/a", Name: "b", IsDir: true}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := c.BatchIndex(ctx, []ext.SearchNode{
		{Parent: "/a/b", Name: "c.txt", Size: 3},
		{Parent: "/a/b", Name: "d.txt", Size: 5},
	}); err != nil {
		t.Fatalf("BatchIndex: %v", err)
	}
	// A sibling OUTSIDE the /a/b subtree, so the subtree Del below can be
	// proven to leave it untouched.
	if err := c.Index(ctx, ext.SearchNode{Parent: "/a", Name: "e.txt", Size: 7}); err != nil {
		t.Fatalf("Index sibling: %v", err)
	}

	got, err := c.Get(ctx, "/a/b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Get(/a/b) = %d nodes, want 2 (BatchIndex must have crossed the wire intact): %+v", len(got), got)
	}

	nodes, total, err := c.Search(ctx, ext.SearchReq{Keywords: "c.txt", PerPage: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total != 1 || len(nodes) != 1 || nodes[0].Name != "c.txt" || nodes[0].Size != 3 {
		t.Fatalf("Search(c.txt) = %+v, total=%d, want one c.txt/size=3 hit", nodes, total)
	}

	if err := c.Del(ctx, "/a/b"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	// Del(/a/b) removes the dir node /a/b and both children under it, leaving
	// only the /a/e.txt sibling.
	afterDel, _, err := c.Search(ctx, ext.SearchReq{})
	if err != nil {
		t.Fatalf("Search after Del: %v", err)
	}
	if len(afterDel) != 1 || afterDel[0].Name != "e.txt" {
		t.Fatalf("after Del(/a/b) search = %+v, want only the /a/e.txt sibling", afterDel)
	}

	if err := c.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	afterClear, totalClear, err := c.Search(ctx, ext.SearchReq{})
	if err != nil {
		t.Fatalf("Search after Clear: %v", err)
	}
	if len(afterClear) != 0 || totalClear != 0 {
		t.Fatalf("after Clear search = %+v total=%d, want empty", afterClear, totalClear)
	}
}

// TestSearchIndexerGRPCFindByNameThroughExtension drives the full host-side
// path this Phase E ring 1b delivers: extension.SetSearchIndexerFactory ->
// extension.FindSearchIndexerByName -> a real gRPC subprocess's
// Index/Search, exactly the wiring server/handles/plugins.go installs in
// production (with pluginproc.Default.AcquireSearchIndexer in place of this
// test's direct host.LaunchSearchIndexer), and internal/search's
// ActiveIndexer own miss-fallback in place of calling FindSearchIndexerByName
// directly. Mirrors TestOfflineDownloadToolGRPCFindByNameThroughExtension one
// Kind over.
func TestSearchIndexerGRPCFindByNameThroughExtension(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchSearchIndexerPlugin(t)

	extension.SetSearchIndexerFactory(func(e *extension.Entry) (ext.SearchIndexer, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetSearchIndexerFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testsearchindexer",
		Kinds: []ext.Kind{ext.KindSearchIndexer},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	indexer, ok := extension.FindSearchIndexerByName(r, "echoindexer")
	if !ok || indexer == nil {
		t.Fatalf("FindSearchIndexerByName(echoindexer) = %v, %v; want a resolved indexer", indexer, ok)
	}

	ctx := context.Background()
	if err := indexer.Index(ctx, ext.SearchNode{Parent: "/x", Name: "y.txt", Size: 9}); err != nil {
		t.Fatalf("Index through the gRPC-resolved indexer: %v", err)
	}
	nodes, total, err := indexer.Search(ctx, ext.SearchReq{Keywords: "y.txt"})
	if err != nil {
		t.Fatalf("Search through the gRPC-resolved indexer: %v", err)
	}
	if total != 1 || len(nodes) != 1 || nodes[0].Name != "y.txt" {
		t.Fatalf("Search(y.txt) = %+v total=%d, want one y.txt hit", nodes, total)
	}
}
