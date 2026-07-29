package ext

import (
	"context"
	"encoding/json"
	stdpath "path"
	"testing"
	"time"
)

// fakeSearchIndexer is a minimal in-process ext.SearchIndexer that pins the
// interface's compile-time shape. It keeps an in-memory node table keyed by
// virtual path so Index/Get/Del/Clear/Search behave enough to exercise the base.
type fakeSearchIndexer struct {
	nodes map[string]SearchNode
}

func newFakeSearchIndexer() *fakeSearchIndexer {
	return &fakeSearchIndexer{nodes: map[string]SearchNode{}}
}

func (f *fakeSearchIndexer) Manifest() Manifest                     { return Manifest{ID: "builtin.search.fake"} }
func (f *fakeSearchIndexer) Init(ctx context.Context, h Host) error { return nil }
func (f *fakeSearchIndexer) Shutdown(ctx context.Context) error     { return nil }

func (f *fakeSearchIndexer) Config() SearchIndexerConfig {
	return SearchIndexerConfig{Name: "fake", AutoUpdate: true}
}

func vpath(n SearchNode) string { return stdpath.Join(n.Parent, n.Name) }

func (f *fakeSearchIndexer) Index(ctx context.Context, node SearchNode) error {
	f.nodes[vpath(node)] = node
	return nil
}

func (f *fakeSearchIndexer) BatchIndex(ctx context.Context, nodes []SearchNode) error {
	for _, n := range nodes {
		f.nodes[vpath(n)] = n
	}
	return nil
}

func (f *fakeSearchIndexer) Search(ctx context.Context, req SearchReq) ([]SearchNode, int64, error) {
	var out []SearchNode
	for _, n := range f.nodes {
		out = append(out, n)
	}
	return out, int64(len(out)), nil
}

func (f *fakeSearchIndexer) Get(ctx context.Context, parent string) ([]SearchNode, error) {
	var out []SearchNode
	for _, n := range f.nodes {
		if n.Parent == parent {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeSearchIndexer) Del(ctx context.Context, prefix string) error {
	for k := range f.nodes {
		if k == prefix || len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(f.nodes, k)
		}
	}
	return nil
}

func (f *fakeSearchIndexer) Clear(ctx context.Context) error {
	f.nodes = map[string]SearchNode{}
	return nil
}

// Compile-time proof the fake satisfies the extension point.
var _ SearchIndexer = (*fakeSearchIndexer)(nil)

func TestSearchIndexerInterfaceShape(t *testing.T) {
	var idx SearchIndexer = newFakeSearchIndexer()
	if idx.Config().Name != "fake" || !idx.Config().AutoUpdate {
		t.Fatalf("Config = %+v", idx.Config())
	}
	ctx := context.Background()
	if err := idx.BatchIndex(ctx, []SearchNode{
		{Parent: "/a", Name: "b", IsDir: true},
		{Parent: "/a/b", Name: "c.txt", Size: 3},
	}); err != nil {
		t.Fatalf("BatchIndex: %v", err)
	}
	if err := idx.Index(ctx, SearchNode{Parent: "/a", Name: "d.txt", Size: 1}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	got, err := idx.Get(ctx, "/a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Get(/a) = %d nodes, want 2", len(got))
	}
	_, total, err := idx.Search(ctx, SearchReq{Keywords: "c", PerPage: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total != 3 {
		t.Fatalf("Search total = %d, want 3", total)
	}
	if err := idx.Del(ctx, "/a/b"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	// Del(/a/b) removes the dir node /a/b and its child /a/b/c.txt, leaving
	// only /a/d.txt.
	if _, total, _ := idx.Search(ctx, SearchReq{}); total != 1 {
		t.Fatalf("after subtree Del total = %d, want 1", total)
	}
	if err := idx.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, total, _ := idx.Search(ctx, SearchReq{}); total != 0 {
		t.Fatalf("after Clear total = %d, want 0", total)
	}
}

// TestSearchNodeNoPathField pins the model: the virtual path is derived from
// Parent+Name, so the JSON has parent/name/is_dir/size and NO path key (mirrors
// v3 model.SearchNode, keeping the index single-keyed per object).
func TestSearchNodeNoPathField(t *testing.T) {
	b, err := json.Marshal(SearchNode{Parent: "/a", Name: "b.txt", IsDir: false, Size: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"parent", "name", "is_dir", "size"} {
		if _, ok := m[want]; !ok {
			t.Errorf("SearchNode JSON missing %q: %s", want, b)
		}
	}
	if _, ok := m["path"]; ok {
		t.Errorf("SearchNode JSON must NOT carry a path field: %s", b)
	}
}

// TestIndexProgressRoundTrip pins the progress snapshot shape the host persists
// and the frontend polls.
func TestIndexProgressRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	p := IndexProgress{ObjCount: 42, IsDone: true, LastDoneTime: &now, Error: ""}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got IndexProgress
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ObjCount != 42 || !got.IsDone || got.LastDoneTime == nil || !got.LastDoneTime.Equal(now) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
