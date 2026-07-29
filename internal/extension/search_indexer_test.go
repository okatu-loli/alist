package extension

import (
	"context"
	"reflect"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// stubSearchIndexer is a minimal ext.SearchIndexer for registry tests. Only
// Config().Name matters here; the data methods are no-ops.
type stubSearchIndexer struct{ name string }

func (s stubSearchIndexer) Manifest() ext.Manifest                     { return ext.Manifest{ID: s.name} }
func (s stubSearchIndexer) Init(ctx context.Context, h ext.Host) error { return nil }
func (s stubSearchIndexer) Shutdown(ctx context.Context) error         { return nil }
func (s stubSearchIndexer) Config() ext.SearchIndexerConfig {
	return ext.SearchIndexerConfig{Name: s.name}
}
func (s stubSearchIndexer) Search(ctx context.Context, req ext.SearchReq) ([]ext.SearchNode, int64, error) {
	return nil, 0, nil
}
func (s stubSearchIndexer) Index(ctx context.Context, node ext.SearchNode) error         { return nil }
func (s stubSearchIndexer) BatchIndex(ctx context.Context, nodes []ext.SearchNode) error { return nil }
func (s stubSearchIndexer) Get(ctx context.Context, parent string) ([]ext.SearchNode, error) {
	return nil, nil
}
func (s stubSearchIndexer) Del(ctx context.Context, prefix string) error { return nil }
func (s stubSearchIndexer) Clear(ctx context.Context) error              { return nil }

func TestSearchIndexerRegistry(t *testing.T) {
	// Clean any state from other tests in this package sharing the registry.
	for _, id := range SearchIndexerInstanceIDs() {
		RegisterTrustedSearchIndexerInstance(id, nil)
	}

	fake := stubSearchIndexer{name: "fake"}
	if _, ok := GetSearchIndexer("fake"); ok {
		t.Fatal("GetSearchIndexer returned an instance before registration")
	}

	RegisterTrustedSearchIndexerInstance("fake", fake)
	got, ok := GetSearchIndexer("fake")
	if !ok {
		t.Fatal("GetSearchIndexer(fake) not found after registration")
	}
	if got.Config().Name != "fake" {
		t.Fatalf("resolved instance Config().Name = %q, want fake", got.Config().Name)
	}

	// nil deregisters.
	RegisterTrustedSearchIndexerInstance("fake", nil)
	if _, ok := GetSearchIndexer("fake"); ok {
		t.Fatal("GetSearchIndexer(fake) still resolves after nil deregistration")
	}
}

func TestSearchIndexerInstanceIDsSorted(t *testing.T) {
	for _, id := range SearchIndexerInstanceIDs() {
		RegisterTrustedSearchIndexerInstance(id, nil)
	}
	RegisterTrustedSearchIndexerInstance("meilisearch", stubSearchIndexer{name: "meilisearch"})
	RegisterTrustedSearchIndexerInstance("bleve", stubSearchIndexer{name: "bleve"})
	RegisterTrustedSearchIndexerInstance("database_non_full_text", stubSearchIndexer{name: "database_non_full_text"})
	t.Cleanup(func() {
		for _, id := range []string{"meilisearch", "bleve", "database_non_full_text"} {
			RegisterTrustedSearchIndexerInstance(id, nil)
		}
	})

	got := SearchIndexerInstanceIDs()
	want := []string{"bleve", "database_non_full_text", "meilisearch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SearchIndexerInstanceIDs() = %v, want sorted %v", got, want)
	}
}
