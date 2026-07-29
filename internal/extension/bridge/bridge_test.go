package bridge_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlistGo/alist-ext"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// The built-in local driver's Init queries a setting, so a DB must exist. Mirror
// the in-memory sqlite setup the op tests use.
func init() {
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("failed to migrate test database: " + err.Error())
	}
}

// localBridge builds a StorageBridge over the built-in local driver, using the
// synthesized manifest the registry produced for it, and creates one instance
// rooted at root.
func localBridge(t *testing.T, root string) (*bridge.StorageBridge, string) {
	t.Helper()
	ctor, err := extension.DefaultRegistry.GetStorageDriver("Local")
	if err != nil {
		t.Fatalf("local driver not registered: %v", err)
	}
	e, _ := extension.DefaultRegistry.Get("Local")
	b := bridge.NewStorageBridge(ctor, e.Manifest)

	const storageID = "1"
	cfg, _ := json.Marshal(map[string]any{
		"root_folder_path": root,
		"mkdir_perm":       "777",
		"recycle_bin_path": "delete permanently",
	})
	if err := b.NewInstance(context.Background(), storageID, cfg); err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = b.Shutdown(context.Background()) })
	return b, storageID
}

func ref(storageID, path string) ext.ObjRef {
	return ext.ObjRef{StorageID: storageID, Anchor: path}
}

// TestLocalThroughExtParity drives the local driver end-to-end through the ext
// contract (ObjRef/Obj/Link/UploadStream) and checks the results against the
// real filesystem — the 9.1 "local exercises the ext types end-to-end" DoD.
func TestLocalThroughExtParity(t *testing.T) {
	root := t.TempDir()
	b, sid := localBridge(t, root)
	ctx := context.Background()

	// MakeDir
	if _, err := b.Mkdir(ctx, ref(sid, root), "sub"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, "sub")); err != nil || !fi.IsDir() {
		t.Fatalf("Mkdir did not create dir: %v", err)
	}

	// Put a file into sub via the pull-model UploadStream
	content := []byte("hello ext world")
	subPath := filepath.Join(root, "sub")
	if _, err := b.Put(ctx, ref(sid, subPath), &memUpload{name: "a.txt", data: content}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(subPath, "a.txt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("Put content mismatch: %q err=%v", got, err)
	}

	// List sub → should contain a.txt with correct size
	objs, err := b.List(ctx, ref(sid, subPath), ext.ListArgs{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 1 || objs[0].Name != "a.txt" || objs[0].Size != int64(len(content)) {
		t.Fatalf("List = %+v", objs)
	}

	// Get a.txt
	o, err := b.Get(ctx, sid, filepath.Join("sub", "a.txt"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if o.Name != "a.txt" || o.IsDir {
		t.Fatalf("Get = %+v", o)
	}

	// Link + OpenRange: stream byte source read-back (9.0 §1)
	filePath := filepath.Join(subPath, "a.txt")
	link, err := b.Link(ctx, ref(sid, filePath), ext.LinkArgs{})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if link.ByteSource != ext.ByteSourceStream || link.StreamID == "" {
		t.Fatalf("expected stream byte source, got %+v", link)
	}
	rc, err := b.OpenRange(ctx, ref(sid, filePath), link.StreamID, ext.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	streamed, _ := io.ReadAll(rc)
	_ = rc.Close()
	b.CloseStream(link.StreamID)
	if string(streamed) != string(content) {
		t.Fatalf("stream read mismatch: %q", streamed)
	}

	// partial range
	link2, _ := b.Link(ctx, ref(sid, filePath), ext.LinkArgs{})
	rc2, err := b.OpenRange(ctx, ref(sid, filePath), link2.StreamID, ext.Range{Start: 6, Length: 3})
	if err != nil {
		t.Fatalf("OpenRange partial: %v", err)
	}
	part, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	b.CloseStream(link2.StreamID)
	if string(part) != "ext" {
		t.Fatalf("partial range = %q, want %q", part, "ext")
	}

	// Rename a.txt -> b.txt
	if _, err := b.Rename(ctx, ref(sid, filePath), "b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subPath, "b.txt")); err != nil {
		t.Fatalf("Rename target missing: %v", err)
	}

	// Move sub/b.txt -> root
	if _, err := b.Move(ctx, ref(sid, filepath.Join(subPath, "b.txt")), ref(sid, root)); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err != nil {
		t.Fatalf("Move target missing: %v", err)
	}

	// Copy root/b.txt -> sub
	if _, err := b.Copy(ctx, ref(sid, filepath.Join(root, "b.txt")), ref(sid, subPath)); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subPath, "b.txt")); err != nil {
		t.Fatalf("Copy target missing: %v", err)
	}

	// Remove root/b.txt
	if err := b.Remove(ctx, ref(sid, filepath.Join(root, "b.txt"))); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("Remove did not delete: %v", err)
	}
}

// TestBridgeDriverConfig confirms the legacy config maps into the ext shape.
func TestBridgeDriverConfig(t *testing.T) {
	ctor, err := extension.DefaultRegistry.GetStorageDriver("Local")
	if err != nil {
		t.Fatalf("local not registered: %v", err)
	}
	e, _ := extension.DefaultRegistry.Get("Local")
	b := bridge.NewStorageBridge(ctor, e.Manifest)
	dc := b.DriverConfig()
	if dc.Name != "Local" || !dc.OnlyLocal || !dc.LocalSort || !dc.NoCache {
		t.Fatalf("DriverConfig = %+v", dc)
	}
	var _ driver.Driver = ctor()
}

// memUpload is an in-memory ext.UploadStream (pull model) for tests.
type memUpload struct {
	name string
	data []byte
}

func (m *memUpload) Info() ext.UploadInfo {
	return ext.UploadInfo{Name: m.name, Size: int64(len(m.data))}
}
func (m *memUpload) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if int(off)+n >= len(m.data) {
		return n, io.EOF
	}
	return n, nil
}
