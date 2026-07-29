package url_tree_test

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/drivers/url_tree"
	"github.com/alist-org/alist/v3/internal/model"
)

func testTree() (*url_tree.Node, error) {
	text := `folder1:
  name1:https://url1
  http://url2
  folder2:
    http://url3
    http://url4
  http://url5
folder3:
  http://url6
  http://url7
http://url8`
	return url_tree.BuildTree(text, false)
}

func TestBuildTree(t *testing.T) {
	node, err := testTree()
	if err != nil {
		t.Errorf("failed to build tree: %+v", err)
	} else {
		t.Logf("tree: %+v", node)
	}
}

func TestGetNode(t *testing.T) {
	root, err := testTree()
	if err != nil {
		t.Errorf("failed to build tree: %+v", err)
		return
	}
	node := url_tree.GetNodeFromRootByPath(root, "/")
	if node != root {
		t.Errorf("got wrong node: %+v", node)
	}
	url3 := url_tree.GetNodeFromRootByPath(root, "/folder1/folder2/url3")
	if url3 != root.Children[0].Children[2].Children[0] {
		t.Errorf("got wrong node: %+v", url3)
	}
}

// TestParseFields checks that a fully-specified file line yields the parsed
// name, size, modified timestamp and url; and that a bare url line falls back
// to the last url segment as its name.
func TestParseFields(t *testing.T) {
	text := `docs:
  readme.md:1024:1700000000:https://example.com/raw/readme.md
  https://example.com/files/plain.bin`
	root, err := url_tree.BuildTree(text, false)
	if err != nil {
		t.Fatalf("failed to build tree: %+v", err)
	}
	named := url_tree.GetNodeFromRootByPath(root, "/docs/readme.md")
	if named == nil {
		t.Fatalf("named node not found")
	}
	if named.Name != "readme.md" || named.Size != 1024 || named.Modified != 1700000000 ||
		named.Url != "https://example.com/raw/readme.md" {
		t.Errorf("wrong parsed fields: %+v", named)
	}
	// a bare url uses its last path segment as name
	bare := url_tree.GetNodeFromRootByPath(root, "/docs/plain.bin")
	if bare == nil {
		t.Fatalf("bare-url node not found")
	}
	if bare.Url != "https://example.com/files/plain.bin" {
		t.Errorf("wrong bare url: %+v", bare)
	}
}

// TestParseErrors checks the parser rejects malformed lines.
func TestParseErrors(t *testing.T) {
	cases := []string{
		"noturl",                        // no scheme
		" odd:https://example.com/a",    // odd indent (1 space)
		"name:notanint:https://x.com/a", // non-integer size
	}
	for _, c := range cases {
		if _, err := url_tree.BuildTree(c, false); err == nil {
			t.Errorf("expected error for line %q, got nil", c)
		}
	}
}

// TestCalSize checks folder sizes are the sum of their descendants. Init
// triggers calSize, so we read the aggregated sizes back through Get.
func TestCalSize(t *testing.T) {
	d := newUrls(t, `folder:
  a:10:https://example.com/a
  b:20:https://example.com/b
  sub:
    c:5:https://example.com/c`)
	ctx := context.Background()
	folder, err := d.Get(ctx, "/folder")
	if err != nil {
		t.Fatalf("get folder: %+v", err)
	}
	if folder.GetSize() != 35 {
		t.Errorf("folder size = %d, want 35", folder.GetSize())
	}
	sub, err := d.Get(ctx, "/folder/sub")
	if err != nil {
		t.Fatalf("get sub: %+v", err)
	}
	if sub.GetSize() != 5 {
		t.Errorf("sub size = %d, want 5", sub.GetSize())
	}
}

func newUrls(t *testing.T, structure string) *url_tree.Urls {
	t.Helper()
	d := &url_tree.Urls{
		Addition: url_tree.Addition{UrlStructure: structure},
	}
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("failed to init: %+v", err)
	}
	return d
}

// TestListAndGet exercises List/Get over a parsed tree via the driver.
func TestListAndGet(t *testing.T) {
	d := newUrls(t, `folder1:
  name1:https://url1
  http://url2
folder3:
  http://url6
http://url8`)
	ctx := context.Background()

	rootObjs, err := d.List(ctx, &model.Object{Path: "/", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatalf("list root: %+v", err)
	}
	got := map[string]bool{}
	for _, o := range rootObjs {
		got[o.GetName()] = true
	}
	for _, want := range []string{"folder1", "folder3", "url8"} {
		if !got[want] {
			t.Errorf("root listing missing %q; got %+v", want, got)
		}
	}

	subObjs, err := d.List(ctx, &model.Object{Path: "/folder1", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatalf("list folder1: %+v", err)
	}
	if len(subObjs) != 2 {
		t.Errorf("folder1 should have 2 children, got %d", len(subObjs))
	}

	// Get a named file resolves through to the node
	f, err := d.Get(ctx, "/folder1/name1")
	if err != nil {
		t.Fatalf("get file: %+v", err)
	}
	if f.GetName() != "name1" || f.IsDir() {
		t.Errorf("wrong file obj: %+v", f)
	}

	// Get a folder is a dir
	dir, err := d.Get(ctx, "/folder1")
	if err != nil {
		t.Fatalf("get folder: %+v", err)
	}
	if !dir.IsDir() {
		t.Errorf("folder1 should be a dir: %+v", dir)
	}

	// Listing a file returns NotFolder
	if _, err := d.List(ctx, &model.Object{Path: "/folder1/name1"}, model.ListArgs{}); err == nil {
		t.Errorf("listing a file should error")
	}
}

// TestLinkReturnsURL checks Link returns the node's url form.
func TestLinkReturnsURL(t *testing.T) {
	d := newUrls(t, `name1:https://cdn.example.com/name1
folder:
  http://cdn.example.com/inner`)
	ctx := context.Background()

	f, err := d.Get(ctx, "/name1")
	if err != nil {
		t.Fatalf("get file: %+v", err)
	}
	link, err := d.Link(ctx, f, model.LinkArgs{})
	if err != nil {
		t.Fatalf("link: %+v", err)
	}
	if link.URL != "https://cdn.example.com/name1" {
		t.Errorf("link url = %s, want https://cdn.example.com/name1", link.URL)
	}

	// Link on a folder returns NotFile
	dir, err := d.Get(ctx, "/folder")
	if err != nil {
		t.Fatalf("get folder: %+v", err)
	}
	if _, err := d.Link(ctx, dir, model.LinkArgs{}); err == nil {
		t.Errorf("link on folder should error")
	}
}

func TestDownloadParams(t *testing.T) {
	d := &url_tree.Urls{
		Addition: url_tree.Addition{
			UrlStructure:   "测试文件.mp4:https://cdn.example.com/random123",
			DownloadParams: "attname={{.Name}}",
		},
	}
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("failed to init: %+v", err)
	}
	file, err := d.Get(context.Background(), "/测试文件.mp4")
	if err != nil {
		t.Fatalf("failed to get file: %+v", err)
	}

	// preview must keep the original (clean) URL
	previewLink, err := d.Link(context.Background(), file, model.LinkArgs{Type: "preview"})
	if err != nil {
		t.Fatalf("failed to get preview link: %+v", err)
	}
	if previewLink.URL != "https://cdn.example.com/random123" {
		t.Errorf("preview link should be clean, got: %s", previewLink.URL)
	}

	// download must append the encoded custom parameters
	downLink, err := d.Link(context.Background(), file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("failed to get download link: %+v", err)
	}
	want := "https://cdn.example.com/random123?attname=%E6%B5%8B%E8%AF%95%E6%96%87%E4%BB%B6.mp4"
	if downLink.URL != want {
		t.Errorf("download link mismatch:\n got: %s\nwant: %s", downLink.URL, want)
	}
}

// TestStringifyRoundTrip checks StringifyTree reproduces a structure that
// re-parses to the same shape.
func TestStringifyRoundTrip(t *testing.T) {
	text := `folder1:
  name1:https://url1
  folder2:
    http://url3
http://url8`
	root, err := url_tree.BuildTree(text, false)
	if err != nil {
		t.Fatalf("build: %+v", err)
	}
	out := url_tree.StringifyTree(root)
	root2, err := url_tree.BuildTree(out, false)
	if err != nil {
		t.Fatalf("re-build from stringified: %+v\n%s", err, out)
	}
	n := url_tree.GetNodeFromRootByPath(root2, "/folder1/folder2/url3")
	if n == nil || n.Url != "http://url3" {
		t.Errorf("round-trip lost node; stringified:\n%s", out)
	}
}
