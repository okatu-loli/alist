package bootstrap

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/search"
	"github.com/alist-org/alist/v3/internal/search/bleve"
	"github.com/alist-org/alist/v3/internal/search/database"
	"github.com/alist-org/alist/v3/internal/search/meilisearch"
)

// cleanupSearchBackends deregisters every backend InitSearchBackends wires up and
// resets the base to the BFS fallback, so each test starts from a clean registry.
func cleanupSearchBackends() {
	extension.DefaultRegistry.Deregister(database.ExtensionID)
	extension.DefaultRegistry.Deregister(bleve.ExtensionID)
	extension.DefaultRegistry.Deregister(meilisearch.ExtensionID)
	search.RegisterBackend(database.SearcherName, database.ExtensionID, nil)
	search.RegisterBackend(bleve.SearcherName, bleve.ExtensionID, nil)
	search.RegisterBackend(meilisearch.SearcherName, meilisearch.ExtensionID, nil)
	_ = search.Init("none")
}

// TestInitSearchBackends pins the A2 wiring: builtin.search.database registers
// as an enabled, manageable Kind=SearchIndexer entry AND a trusted in-process
// instance; the search base resolves it; and disabling the extension gates the
// active indexer off (search falls back to BFS) without any manager hook.
func TestInitSearchBackends(t *testing.T) {
	t.Cleanup(cleanupSearchBackends)

	InitSearchBackends()

	// (1) manageable entry: enabled, Kind SearchIndexer.
	e, ok := extension.DefaultRegistry.Get(database.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", database.ExtensionID)
	}
	if !e.Enabled() {
		t.Fatal("backend extension must be enabled by default")
	}
	if !e.HasKind(ext.KindSearchIndexer) {
		t.Fatal("backend extension must carry ext.KindSearchIndexer")
	}

	// (2) trusted instance resolvable by searcher name.
	if _, ok := extension.GetSearchIndexer(database.SearcherName); !ok {
		t.Fatalf("GetSearchIndexer(%q) must return the instance", database.SearcherName)
	}

	// (3) base resolves it -> ActiveIndexer non-nil.
	if err := search.Init(database.SearcherName); err != nil {
		t.Fatalf("search.Init: %v", err)
	}
	if search.ActiveIndexer() == nil {
		t.Fatal("ActiveIndexer must be non-nil while the backend extension is enabled")
	}

	// (4) enabled-gate: disabling the OWNING extension makes ActiveIndexer nil
	// (search falls back to BFS), with no manager->search callback.
	if err := extension.DefaultRegistry.Disable(database.ExtensionID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if search.ActiveIndexer() != nil {
		t.Fatal("ActiveIndexer must be nil when the backend extension is disabled (gated)")
	}

	// (5) re-enabling restores it.
	if err := extension.DefaultRegistry.Enable(database.ExtensionID); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if search.ActiveIndexer() == nil {
		t.Fatal("ActiveIndexer must be restored when the backend extension is re-enabled")
	}
}

// TestInitSearchBackendsBleve pins the A5 wiring: builtin.search.bleve registers
// ALONGSIDE builtin.search.database as its own enabled, manageable
// Kind=SearchIndexer entry AND trusted in-process instance; the search base
// resolves it by SearcherName; and disabling the extension gates the active
// indexer off, exactly like the database backend.
func TestInitSearchBackendsBleve(t *testing.T) {
	t.Cleanup(cleanupSearchBackends)

	InitSearchBackends()

	// (1) manageable entry: enabled, Kind SearchIndexer.
	e, ok := extension.DefaultRegistry.Get(bleve.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", bleve.ExtensionID)
	}
	if !e.Enabled() {
		t.Fatal("bleve extension must be enabled by default")
	}
	if !e.HasKind(ext.KindSearchIndexer) {
		t.Fatal("bleve extension must carry ext.KindSearchIndexer")
	}

	// (2) trusted instance resolvable by searcher name.
	if _, ok := extension.GetSearchIndexer(bleve.SearcherName); !ok {
		t.Fatalf("GetSearchIndexer(%q) must return the instance", bleve.SearcherName)
	}

	// (3) base resolves it -> ActiveIndexer non-nil.
	if err := search.Init(bleve.SearcherName); err != nil {
		t.Fatalf("search.Init: %v", err)
	}
	if search.ActiveIndexer() == nil {
		t.Fatal("ActiveIndexer must be non-nil while the bleve extension is enabled")
	}

	// (4) enabled-gate: disabling the OWNING extension makes ActiveIndexer nil.
	if err := extension.DefaultRegistry.Disable(bleve.ExtensionID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if search.ActiveIndexer() != nil {
		t.Fatal("ActiveIndexer must be nil when the bleve extension is disabled (gated)")
	}

	// (5) database backend remains independently registered and selectable.
	if _, ok := extension.GetSearchIndexer(database.SearcherName); !ok {
		t.Fatal("database backend must remain registered alongside bleve")
	}
}

// TestInitSearchBackendsMeilisearch pins the A6 wiring: builtin.search.meilisearch
// registers ALONGSIDE database and bleve as its own enabled, manageable
// Kind=SearchIndexer entry AND trusted in-process instance; the search base
// resolves it by SearcherName; and disabling the extension gates the active
// indexer off, exactly like the other backends — while database and bleve stay
// registered.
func TestInitSearchBackendsMeilisearch(t *testing.T) {
	t.Cleanup(cleanupSearchBackends)

	InitSearchBackends()

	// (1) manageable entry: enabled, Kind SearchIndexer.
	e, ok := extension.DefaultRegistry.Get(meilisearch.ExtensionID)
	if !ok {
		t.Fatalf("%s must be registered", meilisearch.ExtensionID)
	}
	if !e.Enabled() {
		t.Fatal("meilisearch extension must be enabled by default")
	}
	if !e.HasKind(ext.KindSearchIndexer) {
		t.Fatal("meilisearch extension must carry ext.KindSearchIndexer")
	}

	// (2) trusted instance resolvable by searcher name.
	if _, ok := extension.GetSearchIndexer(meilisearch.SearcherName); !ok {
		t.Fatalf("GetSearchIndexer(%q) must return the instance", meilisearch.SearcherName)
	}

	// (3) base resolves it -> ActiveIndexer non-nil.
	if err := search.Init(meilisearch.SearcherName); err != nil {
		t.Fatalf("search.Init: %v", err)
	}
	if search.ActiveIndexer() == nil {
		t.Fatal("ActiveIndexer must be non-nil while the meilisearch extension is enabled")
	}

	// (4) enabled-gate: disabling the OWNING extension makes ActiveIndexer nil.
	if err := extension.DefaultRegistry.Disable(meilisearch.ExtensionID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if search.ActiveIndexer() != nil {
		t.Fatal("ActiveIndexer must be nil when the meilisearch extension is disabled (gated)")
	}

	// (5) database + bleve remain independently registered and selectable.
	if _, ok := extension.GetSearchIndexer(database.SearcherName); !ok {
		t.Fatal("database backend must remain registered alongside meilisearch")
	}
	if _, ok := extension.GetSearchIndexer(bleve.SearcherName); !ok {
		t.Fatal("bleve backend must remain registered alongside meilisearch")
	}
}
