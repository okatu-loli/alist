package hostcap

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// This is the W1 hostFS.Put security + correctness proof: write is authorized
// on the destination dir (invariant 1, same choke point as Mkdir), a
// traversing name is refused (the VULN 1 guard, reused for Put's leaf
// filename), and — the actual byte transfer — the bytes served by the
// plugin's ext.UploadStream land on disk unchanged and the returned Obj
// reports the right name/size.

// byteUploadStream is a minimal ext.UploadStream backed by an in-memory
// slice, standing in for what hostFSClient.Put's caller (a real plugin) would
// hand it. off >= len(data) is the only EOF condition, matching
// remoteUploadStream / uploadStreamReader's convention elsewhere in the repo.
type byteUploadStream struct {
	info ext.UploadInfo
	data []byte
}

func (b *byteUploadStream) Info() ext.UploadInfo { return b.info }

func (b *byteUploadStream) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	return copy(p, b.data[off:]), nil
}

func TestFSPutAuthorizationAndBytes(t *testing.T) {
	mnt := "/put-mnt"
	root := mountLocal(t, mnt)
	ctx := context.Background()
	content := []byte("hello from a plugin upload")

	newUp := func() ext.UploadStream {
		return &byteUploadStream{info: ext.UploadInfo{Name: "put-file.txt", Size: int64(len(content))}, data: content}
	}

	// Read-only grant: Put is denied before any byte is written.
	ro := New(Grant{PluginID: "p.put.ro", HostAPIs: []GrantItem{{Name: CapFS}}, FS: []FSScope{{Path: mnt}}}, Options{})
	if _, err := ro.FS().Put(ctx, "", ext.ObjRef{Anchor: mnt}, newUp()); err != ext.ErrPermissionDenied {
		t.Fatalf("Put read-only grant: want ErrPermissionDenied, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "put-file.txt")); !os.IsNotExist(err) {
		t.Fatalf("Put read-only grant must not have written anything, stat err=%v", err)
	}

	// No FS capability at all: denied (invariant 5).
	nofs := New(Grant{PluginID: "p.put.nofs"}, Options{})
	if _, err := nofs.FS().Put(ctx, "", ext.ObjRef{Anchor: mnt}, newUp()); err != ext.ErrPermissionDenied {
		t.Fatalf("Put without CapFS: want ErrPermissionDenied, got %v", err)
	}

	// Write-granted: the bytes land on disk and the Obj reply matches.
	rw := New(Grant{PluginID: "p.put.rw", HostAPIs: []GrantItem{{Name: CapFS}}, FS: []FSScope{{Path: mnt, Write: true}}}, Options{})
	obj, err := rw.FS().Put(ctx, "", ext.ObjRef{Anchor: mnt}, newUp())
	if err != nil {
		t.Fatalf("Put write grant: %v", err)
	}
	if obj.Name != "put-file.txt" || obj.Size != int64(len(content)) {
		t.Fatalf("Put reply = %+v, want name=put-file.txt size=%d", obj, len(content))
	}
	got, err := os.ReadFile(filepath.Join(root, "put-file.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("written content = %q, want %q", got, content)
	}

	// A traversing name must not escape the authorized destination dir (VULN 1
	// guard reused for Put's leaf filename).
	traversal := &byteUploadStream{info: ext.UploadInfo{Name: "../escaped.txt", Size: int64(len(content))}, data: content}
	if _, err := rw.FS().Put(ctx, "", ext.ObjRef{Anchor: mnt}, traversal); err != ext.ErrPermissionDenied {
		t.Fatalf("Put traversing name: want ErrPermissionDenied, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("Put traversal must not have escaped the mount, stat err=%v", err)
	}
}
