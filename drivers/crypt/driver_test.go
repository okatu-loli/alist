package crypt_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/drivers/crypt"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
)

// boot spins a throwaway sqlite database (auto-migrated, so a download signing
// key is seeded) in a temp DataDir and resets the op-layer caches so entities
// aren't read from a previous test's database. Mirrors the alias driver tests.
func boot(t *testing.T) {
	t.Helper()
	oldDataDir := flags.DataDir
	oldMode := flags.MigrateMode
	t.Cleanup(func() {
		flags.DataDir = oldDataDir
		flags.MigrateMode = oldMode
	})
	flags.DataDir = t.TempDir()
	flags.MigrateMode = "auto"
	bootstrap.InitConfig()
	bootstrap.InitDB()
	op.ResetCachesForTest()
}

func mustCreate(t *testing.T, s model.Storage) {
	t.Helper()
	if _, err := op.CreateStorage(context.Background(), s); err != nil {
		t.Fatalf("create storage %s: %+v", s.MountPath, err)
	}
}

func names(objs []model.Obj) map[string]model.Obj {
	m := make(map[string]model.Obj, len(objs))
	for _, o := range objs {
		m[o.GetName()] = o
	}
	return m
}

const (
	testPassword = "correct-horse-battery-staple"
	testSalt     = "a-second-secret-salt"
)

// cryptAddition builds a Crypt Addition JSON pointing at remotePath with
// standard filename encryption and directory-name encryption on.
func cryptAddition(remotePath, password, salt string) string {
	return `{` +
		`"remote_path":"` + remotePath + `",` +
		`"filename_encryption":"standard",` +
		`"directory_name_encryption":"true",` +
		`"password":"` + password + `",` +
		`"salt":"` + salt + `",` +
		`"encrypted_suffix":".bin",` +
		`"filename_encoding":"base64",` +
		`"thumbnail":false,` +
		`"show_hidden":true}`
}

// setup creates a Local storage over a fresh tempdir (the underlying remote
// that physically stores the ciphertext) and a Crypt storage layered over it.
// It returns the crypt driver and the backing on-disk directory.
func setup(t *testing.T) (*crypt.Crypt, string) {
	boot(t)
	remoteDir := t.TempDir()
	mustCreate(t, model.Storage{Driver: "Local", MountPath: "/remote", Order: 0,
		Addition: `{"root_folder_path":"` + remoteDir + `"}`})
	mustCreate(t, model.Storage{Driver: "Crypt", MountPath: "/sec", Order: 1,
		Addition: cryptAddition("/remote", testPassword, testSalt)})
	d, err := op.GetStorageByMountPath("/sec")
	if err != nil {
		t.Fatal(err)
	}
	return d.(*crypt.Crypt), remoteDir
}

// putFile writes content through the crypt driver into dir (a crypt-internal
// obj) under name.
func putFile(t *testing.T, d *crypt.Crypt, dir model.Obj, name, content string) {
	t.Helper()
	fs := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     int64(len(content)),
			Modified: time.Now(),
		},
		Reader:   io.NopCloser(bytes.NewReader([]byte(content))),
		Mimetype: "text/plain",
	}
	if err := d.Put(context.Background(), dir, fs, func(float64) {}); err != nil {
		t.Fatalf("put %q: %+v", name, err)
	}
}

// readLink reads the full plaintext of file back through the crypt link path.
func readLink(t *testing.T, d *crypt.Crypt, file model.Obj) []byte {
	t.Helper()
	ctx := context.Background()
	link, err := d.Link(ctx, file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("link %q: %+v", file.GetPath(), err)
	}
	if link.RangeReadCloser == nil {
		t.Fatalf("crypt link has no RangeReadCloser: %+v", link)
	}
	rc, err := link.RangeReadCloser.RangeRead(ctx, http_range.Range{Start: 0, Length: -1})
	if err != nil {
		t.Fatalf("range read: %+v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %+v", err)
	}
	return data
}

// walkFiles returns every regular file (relative path -> content) under root.
func walkFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %+v", root, err)
	}
	return out
}

var rootObj = &model.Object{Path: "/", IsFolder: true}

// TestCryptEncryptsOnDisk: a file put through crypt is stored encrypted on the
// underlying disk — neither the plaintext bytes nor the plaintext name appear.
func TestCryptEncryptsOnDisk(t *testing.T) {
	d, remoteDir := setup(t)
	const name = "secret.txt"
	const content = "the quick brown fox jumps over the lazy dog, encrypted"
	putFile(t, d, rootObj, name, content)

	files := walkFiles(t, remoteDir)
	if len(files) == 0 {
		t.Fatal("no file written to underlying remote")
	}
	for rel, b := range files {
		base := filepath.Base(rel)
		if base == name || strings.Contains(base, name) {
			t.Errorf("filename not obfuscated on disk: %q", rel)
		}
		if bytes.Contains(b, []byte(content)) {
			t.Errorf("plaintext content found on disk in %q", rel)
		}
	}
}

// TestCryptListGetDecrypt: listing and Get through crypt expose the decrypted
// name and the true (decrypted) size.
func TestCryptListGetDecrypt(t *testing.T) {
	d, _ := setup(t)
	ctx := context.Background()
	const name = "notes.txt"
	const content = "hello encrypted world"
	putFile(t, d, rootObj, name, content)

	objs, err := d.List(ctx, rootObj, model.ListArgs{})
	if err != nil {
		t.Fatalf("list: %+v", err)
	}
	n := names(objs)
	obj := n[name]
	if obj == nil {
		t.Fatalf("decrypted name %q not in listing; got %v", name, keys(n))
	}
	if obj.GetSize() != int64(len(content)) {
		t.Errorf("listed size = %d, want %d", obj.GetSize(), len(content))
	}

	got, err := d.Get(ctx, "/"+name)
	if err != nil {
		t.Fatalf("get: %+v", err)
	}
	if got.GetSize() != int64(len(content)) {
		t.Errorf("Get size = %d, want %d", got.GetSize(), len(content))
	}
	if got.IsDir() {
		t.Errorf("Get returned a dir for a file")
	}
}

// TestCryptRoundTrip: reading a file back through the crypt link yields the
// exact original plaintext, byte-for-byte.
func TestCryptRoundTrip(t *testing.T) {
	d, _ := setup(t)
	ctx := context.Background()
	const name = "roundtrip.bin"
	// Use content spanning more than one crypt block boundary (64KiB blocks).
	content := strings.Repeat("0123456789abcdef", 8000) // 128000 bytes
	putFile(t, d, rootObj, name, content)

	obj, err := d.Get(ctx, "/"+name)
	if err != nil {
		t.Fatalf("get: %+v", err)
	}
	got := readLink(t, d, obj)
	if !bytes.Equal(got, []byte(content)) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// TestCryptWrongPassword: a second crypt mount over the same remote with the
// wrong password cannot see or resolve the file — it fails loudly.
func TestCryptWrongPassword(t *testing.T) {
	d, _ := setup(t)
	ctx := context.Background()
	const name = "topsecret.txt"
	const content = "you shall not read this"
	putFile(t, d, rootObj, name, content)

	// sanity: correct mount sees it
	if _, err := d.Get(ctx, "/"+name); err != nil {
		t.Fatalf("correct mount cannot get its own file: %+v", err)
	}

	mustCreate(t, model.Storage{Driver: "Crypt", MountPath: "/bad", Order: 2,
		Addition: cryptAddition("/remote", "the-wrong-password", testSalt)})
	bd, err := op.GetStorageByMountPath("/bad")
	if err != nil {
		t.Fatal(err)
	}
	bad := bd.(*crypt.Crypt)

	// With the wrong key the encrypted name can't be decrypted, so it is
	// filtered out of the listing entirely.
	objs, err := bad.List(ctx, rootObj, model.ListArgs{})
	if err != nil {
		t.Fatalf("bad list: %+v", err)
	}
	if n := names(objs); n[name] != nil {
		t.Errorf("wrong-password mount should not see %q, got %v", name, keys(n))
	}

	// And Get must fail: the wrong key encrypts the requested name differently,
	// so no matching object exists on the remote.
	if _, err := bad.Get(ctx, "/"+name); err == nil {
		t.Errorf("wrong-password Get unexpectedly succeeded")
	}
}

// TestCryptMakeDirRenameRemove: directory create, rename and remove all pass
// through to the encrypted remote and are reflected in the decrypted listing.
func TestCryptMakeDirRenameRemove(t *testing.T) {
	d, _ := setup(t)
	ctx := context.Background()

	// MakeDir
	if err := d.MakeDir(ctx, rootObj, "folder"); err != nil {
		t.Fatalf("makedir: %+v", err)
	}
	objs, err := d.List(ctx, rootObj, model.ListArgs{})
	if err != nil {
		t.Fatalf("list after makedir: %+v", err)
	}
	dirObj := names(objs)["folder"]
	if dirObj == nil || !dirObj.IsDir() {
		t.Fatalf("created dir not listed as folder; got %v", keys(names(objs)))
	}

	// Put a file, then Rename it
	putFile(t, d, rootObj, "before.txt", "data")
	src, err := d.Get(ctx, "/before.txt")
	if err != nil {
		t.Fatalf("get before.txt: %+v", err)
	}
	if err := d.Rename(ctx, src, "after.txt"); err != nil {
		t.Fatalf("rename: %+v", err)
	}
	objs, _ = d.List(ctx, rootObj, model.ListArgs{})
	n := names(objs)
	if n["before.txt"] != nil {
		t.Errorf("old name still present after rename")
	}
	if n["after.txt"] == nil {
		t.Errorf("new name missing after rename; got %v", keys(n))
	}

	// content survives the rename (still decryptable)
	renamed, err := d.Get(ctx, "/after.txt")
	if err != nil {
		t.Fatalf("get after.txt: %+v", err)
	}
	if got := readLink(t, d, renamed); string(got) != "data" {
		t.Errorf("renamed content = %q, want %q", got, "data")
	}

	// Remove
	if err := d.Remove(ctx, renamed); err != nil {
		t.Fatalf("remove: %+v", err)
	}
	objs, _ = d.List(ctx, rootObj, model.ListArgs{})
	if names(objs)["after.txt"] != nil {
		t.Errorf("file still present after remove")
	}
}

func keys(m map[string]model.Obj) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
