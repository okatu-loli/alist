package alias_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/drivers/alias"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// boot spins a throwaway sqlite database (auto-migrated, so a download signing
// key is seeded) in a temp DataDir and resets the op-layer caches so entities
// aren't read from a previous test's database.
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

// writeFile creates a file with the given content, making parent dirs.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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

// setup builds two Local storages (/la over dirA, /lb over dirB) with files,
// then returns the two backing dirs. dirA has a1.txt, common.txt (fromA) and a
// shared/ subdir; dirB has b1.txt and common.txt (fromB).
func setup(t *testing.T) (dirA, dirB string) {
	boot(t)
	dirA = t.TempDir()
	dirB = t.TempDir()
	writeFile(t, dirA, "a1.txt", "aaa")
	writeFile(t, dirA, "common.txt", "fromA")
	writeFile(t, dirA, "shared/x.txt", "x")
	writeFile(t, dirB, "b1.txt", "bbbb")
	writeFile(t, dirB, "common.txt", "fromB")

	mustCreate(t, model.Storage{Driver: "Local", MountPath: "/la", Order: 0,
		Addition: `{"root_folder_path":"` + dirA + `"}`})
	mustCreate(t, model.Storage{Driver: "Local", MountPath: "/lb", Order: 1,
		Addition: `{"root_folder_path":"` + dirB + `"}`})
	return dirA, dirB
}

// TestAliasMultiKeyRoot: two distinct keys => the root lists the keys as
// folders (no auto-flatten), and each key lists its single target's contents.
func TestAliasMultiKeyRoot(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mustCreate(t, model.Storage{Driver: "Alias", MountPath: "/agg",
		Addition: `{"paths":"la:/la\nlb:/lb","protect_same_name":true}`})

	d, err := op.GetStorageByMountPath("/agg")
	if err != nil {
		t.Fatal(err)
	}
	a := d.(*alias.Alias)

	root, err := a.List(ctx, &model.Object{Path: "/", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatalf("list root: %+v", err)
	}
	rn := names(root)
	if len(rn) != 2 || rn["la"] == nil || rn["lb"] == nil {
		t.Fatalf("root should list keys la,lb; got %v", keys(rn))
	}
	if !rn["la"].IsDir() {
		t.Errorf("alias key should be a folder")
	}

	// list one key -> the target storage's contents
	la, err := a.List(ctx, &model.Object{Path: "/la", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatalf("list /la: %+v", err)
	}
	ln := names(la)
	for _, want := range []string{"a1.txt", "common.txt", "shared"} {
		if ln[want] == nil {
			t.Errorf("/la listing missing %q; got %v", want, keys(ln))
		}
	}

	// Get resolves through to the underlying object (size of a1.txt == 3)
	obj, err := a.Get(ctx, "/la/a1.txt")
	if err != nil {
		t.Fatalf("get /la/a1.txt: %+v", err)
	}
	if obj.IsDir() || obj.GetSize() != 3 {
		t.Errorf("wrong resolved obj: dir=%v size=%d", obj.IsDir(), obj.GetSize())
	}
}

// TestAliasMergeFlatten: one key with two targets => auto-flatten merges both
// targets' contents at the root, and Get resolves to whichever target holds it.
func TestAliasMergeFlatten(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mustCreate(t, model.Storage{Driver: "Alias", MountPath: "/merge",
		Addition: `{"paths":"data:/la\ndata:/lb","protect_same_name":true}`})
	d, err := op.GetStorageByMountPath("/merge")
	if err != nil {
		t.Fatal(err)
	}
	a := d.(*alias.Alias)

	root, err := a.List(ctx, &model.Object{Path: "/", IsFolder: true}, model.ListArgs{})
	if err != nil {
		t.Fatalf("list merged root: %+v", err)
	}
	rn := names(root)
	// a1.txt only in A, b1.txt only in B — both must appear (merge)
	if rn["a1.txt"] == nil || rn["b1.txt"] == nil {
		t.Errorf("merged root missing a1.txt/b1.txt; got %v", keys(rn))
	}

	// Get resolves through the second target when absent from the first
	b, err := a.Get(ctx, "/b1.txt")
	if err != nil {
		t.Fatalf("get /b1.txt: %+v", err)
	}
	if b.GetSize() != 4 {
		t.Errorf("b1.txt size = %d, want 4", b.GetSize())
	}
}

// TestAliasLinkProxies: Link resolves through to the underlying storage's link.
// The underlying Local storage serves files by exposing an MFile handle rather
// than a URL, so the alias's Link delegates to op.Link and returns that handle —
// reading it back yields the real underlying file's bytes.
func TestAliasLinkProxies(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mustCreate(t, model.Storage{Driver: "Alias", MountPath: "/merge",
		Addition: `{"paths":"data:/la\ndata:/lb","protect_same_name":true}`})
	d, err := op.GetStorageByMountPath("/merge")
	if err != nil {
		t.Fatal(err)
	}
	a := d.(*alias.Alias)

	file, err := a.Get(ctx, "/a1.txt")
	if err != nil {
		t.Fatalf("get /a1.txt: %+v", err)
	}
	link, err := a.Link(ctx, file, model.LinkArgs{})
	if err != nil {
		t.Fatalf("link: %+v", err)
	}
	if link == nil || link.MFile == nil {
		t.Fatalf("expected an underlying MFile link, got %+v", link)
	}
	buf := make([]byte, 3)
	if _, err := link.MFile.Read(buf); err != nil {
		t.Fatalf("read MFile: %+v", err)
	}
	if string(buf) != "aaa" {
		t.Errorf("link resolved to wrong file, read %q want %q", string(buf), "aaa")
	}
}

// TestAliasWritableGuard: without writable, mutations are denied.
func TestAliasWritableGuard(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mustCreate(t, model.Storage{Driver: "Alias", MountPath: "/ro",
		Addition: `{"paths":"data:/la","protect_same_name":true,"writable":false}`})
	d, err := op.GetStorageByMountPath("/ro")
	if err != nil {
		t.Fatal(err)
	}
	a := d.(*alias.Alias)

	if err := a.MakeDir(ctx, &model.Object{Path: "/", IsFolder: true}, "new"); err != errs.PermissionDenied {
		t.Errorf("MakeDir on read-only alias = %v, want PermissionDenied", err)
	}
	if err := a.Remove(ctx, &model.Object{Path: "/a1.txt"}); err != errs.PermissionDenied {
		t.Errorf("Remove on read-only alias = %v, want PermissionDenied", err)
	}
}

// TestAliasWritablePassthrough: with writable, MakeDir passes through to the
// single underlying target and a rename of a unique file succeeds on disk.
func TestAliasWritablePassthrough(t *testing.T) {
	dirA, _ := setup(t)
	ctx := context.Background()
	mustCreate(t, model.Storage{Driver: "Alias", MountPath: "/w",
		Addition: `{"paths":"data:/la","protect_same_name":true,"writable":true}`})
	d, err := op.GetStorageByMountPath("/w")
	if err != nil {
		t.Fatal(err)
	}
	a := d.(*alias.Alias)

	if err := a.MakeDir(ctx, &model.Object{Path: "/", IsFolder: true}, "newdir"); err != nil {
		t.Fatalf("MakeDir: %+v", err)
	}
	if st, err := os.Stat(filepath.Join(dirA, "newdir")); err != nil || !st.IsDir() {
		t.Errorf("newdir not created on disk under target: err=%v", err)
	}

	// rename a file that exists in only one target
	if err := a.Rename(ctx, &model.Object{Path: "/a1.txt"}, "a1-renamed.txt"); err != nil {
		t.Fatalf("Rename: %+v", err)
	}
	if _, err := os.Stat(filepath.Join(dirA, "a1-renamed.txt")); err != nil {
		t.Errorf("renamed file missing on disk: %v", err)
	}
}

// TestAliasProtectSameName: with two targets both holding common.txt and
// protect_same_name on, a rename of the ambiguous file is refused.
func TestAliasProtectSameName(t *testing.T) {
	setup(t)
	ctx := context.Background()
	mustCreate(t, model.Storage{Driver: "Alias", MountPath: "/wm",
		Addition: `{"paths":"data:/la\ndata:/lb","protect_same_name":true,"writable":true}`})
	d, err := op.GetStorageByMountPath("/wm")
	if err != nil {
		t.Fatal(err)
	}
	a := d.(*alias.Alias)

	err = a.Rename(ctx, &model.Object{Path: "/common.txt"}, "renamed.txt")
	if err == nil || !strings.Contains(err.Error(), "same-name") {
		t.Errorf("rename of ambiguous same-name file = %v, want a same-name refusal", err)
	}

	// a file unique to one target can still be renamed through the same alias
	if err := a.Rename(ctx, &model.Object{Path: "/b1.txt"}, "b1-renamed.txt"); err != nil {
		t.Errorf("rename of unique file should succeed, got %v", err)
	}
}

func keys(m map[string]model.Obj) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
