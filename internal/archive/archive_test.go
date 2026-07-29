package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	// Import the Local driver directly (init side effect) so op.CreateStorage
	// can back the fixture -- mirrors internal/thumbnail/thumbnail_test.go.
	ext "github.com/AlistGo/alist-ext"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:archive_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	// The remote (gRPC) dispatch path's localizeToTemp writes under
	// conf.Conf.TempDir -- conf.DefaultConfig's default is a relative path
	// under the (production) data dir, which would otherwise litter this
	// package's own source directory with real temp files every time the
	// dispatch tests below exercise that path. Point it at a real OS temp dir
	// instead, mirroring how a real process's bootstrap.InitConfig resolves
	// it to an absolute, already-created directory before any request.
	if dir, err := os.MkdirTemp("", "alist-archive-test-*"); err == nil {
		conf.Conf.TempDir = dir
	}
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}

	// Mirror internal/bootstrap.InitArchiveTools: this package's own tests
	// dispatch through internal/extension now (Phase E wave 2), so the
	// built-in zip ArchiveTool must be registered exactly like production
	// bootstrap does, or every Meta/List/Extract call below sees "no tool
	// claims .zip" (ErrNotArchive) before ever reaching the zip-specific
	// logic under test.
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    ZipToolExtensionID,
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}); err != nil {
		panic(err)
	}
	extension.RegisterTrustedArchiveToolInstance(ZipToolExtensionID, NewZipArchiveTool())
}

// ---- fixture zip construction ----

type zipEntrySpec struct {
	name  string
	data  []byte
	dir   bool
	flags uint16 // extra FileHeader.Flags bits to OR in (e.g. 0x1 to simulate an encrypted entry)
}

// buildZip writes entries with archive/zip's real writer -- names are passed
// through UNSANITIZED (the writer never validates them), which is exactly
// what lets the zip-slip fixture entry ("../evil.txt") land in the archive
// the same way a maliciously crafted real-world zip would.
func buildZip(t *testing.T, entries []zipEntrySpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		name := e.name
		if e.dir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if e.flags != 0 {
			fh.Flags |= e.flags
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", name, err)
		}
		if !e.dir {
			if _, err := w.Write(e.data); err != nil {
				t.Fatalf("write entry %q: %v", name, err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// fixtureEntries is the shared zip layout every test below reasons about:
//
//	a.txt                 "root-a"     (root file)
//	dir1/b.txt             "dir1-b"    (nested file, dir1 has no explicit entry -- implicit dir)
//	dir1/dir2/c.txt         "nested-c" (doubly-nested file -- implicit dir1/dir2)
//	emptydir/               (explicit, empty directory entry)
//	secret.bin              "shh"      (Flags|=0x1 -- simulated encrypted entry)
//	../evil.txt              "slip"    (zip-slip attempt: must never be listed/addressable)
func fixtureEntries() []zipEntrySpec {
	return []zipEntrySpec{
		{name: "a.txt", data: []byte("root-a")},
		{name: "dir1/b.txt", data: []byte("dir1-b")},
		{name: "dir1/dir2/c.txt", data: []byte("nested-c")},
		{name: "emptydir", dir: true},
		{name: "secret.bin", data: []byte("shh"), flags: 0x1},
		{name: "../evil.txt", data: []byte("slip")},
	}
}

var archiveFixtureSeq int64

// setupArchiveFixture mounts an isolated Local storage containing
// archive.zip (fixtureEntries), a plain notes.txt, a directory literally
// named adir.zip, and a fake.zip that isn't real zip content -- and returns a
// caller scoped to full recursive ActRead on the mount.
func setupArchiveFixture(t *testing.T) (reader *model.User, mount string, zipBytes []byte) {
	t.Helper()
	n := atomic.AddInt64(&archiveFixtureSeq, 1)
	mount = fmt.Sprintf("/archive%d", n)

	rootDir := t.TempDir()
	zipBytes = buildZip(t, fixtureEntries())
	if err := os.WriteFile(filepath.Join(rootDir, "archive.zip"), zipBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "fake.zip"), []byte("this is not a zip file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "adir.zip"), 0o755); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + rootDir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	reader = &model.User{
		ID:       80000 + uint(n),
		Username: fmt.Sprintf("archivereader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      mount,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}
	return reader, mount, zipBytes
}

func scopedNoAccessReader(id uint) *model.User {
	return &model.User{
		ID:       id,
		Username: fmt.Sprintf("archivenoaccess%d", id),
		Scoped:   true,
	}
}

// ---- Meta ----

func TestMetaCountsEntriesAndDeclaresEncrypted(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	m, err := Meta(context.Background(), reader, mount+"/archive.zip", "")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	// a.txt, dir1/b.txt, dir1/dir2/c.txt, secret.bin = 4 addressable files;
	// emptydir is a directory (not counted); ../evil.txt is excluded entirely
	// (zip-slip).
	if m.EntryCount != 4 {
		t.Fatalf("EntryCount = %d, want 4", m.EntryCount)
	}
	wantSize := int64(len("root-a") + len("dir1-b") + len("nested-c") + len("shh"))
	if m.TotalUncompressedSize != wantSize {
		t.Fatalf("TotalUncompressedSize = %d, want %d", m.TotalUncompressedSize, wantSize)
	}
	if !m.Encrypted {
		t.Fatal("Encrypted = false, want true (secret.bin carries the encrypted flag)")
	}
}

func TestMetaNonZipExtensionRefused(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, err := Meta(context.Background(), reader, mount+"/notes.txt", "")
	if !errors.Is(err, ErrNotArchive) {
		t.Fatalf("Meta(notes.txt) error = %v, want ErrNotArchive", err)
	}
}

func TestMetaCorruptZipContentRefused(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, err := Meta(context.Background(), reader, mount+"/fake.zip", "")
	if !errors.Is(err, ErrNotArchive) {
		t.Fatalf("Meta(fake.zip) error = %v, want ErrNotArchive", err)
	}
}

func TestMetaDirectoryNamedDotZipRefused(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, err := Meta(context.Background(), reader, mount+"/adir.zip", "")
	if !errors.Is(err, ErrNotArchive) {
		t.Fatalf("Meta(adir.zip directory) error = %v, want ErrNotArchive", err)
	}
}

func TestMetaUnauthorizedDenied(t *testing.T) {
	_, mount, _ := setupArchiveFixture(t)
	noAccess := scopedNoAccessReader(80500)
	_, err := Meta(context.Background(), noAccess, mount+"/archive.zip", "")
	if err == nil {
		t.Fatal("Meta for a caller with no ActRead grant must be denied, got nil error")
	}
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("Meta unauthorized error = %v, want errs.PermissionDenied", err)
	}
}

// ---- List ----

func entryNames(t *testing.T, entries []Entry) map[string]Entry {
	t.Helper()
	m := make(map[string]Entry, len(entries))
	for _, e := range entries {
		m[e.Name] = e
	}
	return m
}

func TestListRoot(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	entries, err := List(context.Background(), reader, mount+"/archive.zip", "", "")
	if err != nil {
		t.Fatalf("List(root): %v", err)
	}
	byName := entryNames(t, entries)

	if _, ok := byName["../evil.txt"]; ok {
		t.Fatal("zip-slip entry must never be listed under its raw name")
	}
	if _, ok := byName["evil.txt"]; ok {
		t.Fatal("zip-slip entry must never be listed under its naively-normalized name either")
	}
	if len(byName) != 4 {
		t.Fatalf("List(root) = %d entries %v, want 4 (a.txt, dir1, emptydir, secret.bin)", len(byName), byName)
	}
	a, ok := byName["a.txt"]
	if !ok || a.IsDir || a.Size != int64(len("root-a")) {
		t.Fatalf("a.txt entry = %+v, ok=%v, want file size %d", a, ok, len("root-a"))
	}
	dir1, ok := byName["dir1"]
	if !ok || !dir1.IsDir {
		t.Fatalf("dir1 entry = %+v, ok=%v, want an implicit directory", dir1, ok)
	}
	empty, ok := byName["emptydir"]
	if !ok || !empty.IsDir {
		t.Fatalf("emptydir entry = %+v, ok=%v, want an explicit directory", empty, ok)
	}
	secret, ok := byName["secret.bin"]
	if !ok || secret.IsDir || secret.Size != int64(len("shh")) {
		t.Fatalf("secret.bin entry = %+v, ok=%v, want file size %d", secret, ok, len("shh"))
	}
}

func TestListSubdir(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	entries, err := List(context.Background(), reader, mount+"/archive.zip", "dir1", "")
	if err != nil {
		t.Fatalf("List(dir1): %v", err)
	}
	byName := entryNames(t, entries)
	if len(byName) != 2 {
		t.Fatalf("List(dir1) = %v, want 2 entries (b.txt, dir2)", byName)
	}
	b, ok := byName["b.txt"]
	if !ok || b.IsDir || b.Size != int64(len("dir1-b")) {
		t.Fatalf("b.txt entry = %+v, ok=%v", b, ok)
	}
	dir2, ok := byName["dir2"]
	if !ok || !dir2.IsDir {
		t.Fatalf("dir2 entry = %+v, ok=%v, want an implicit directory", dir2, ok)
	}
}

func TestListNestedSubdir(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	entries, err := List(context.Background(), reader, mount+"/archive.zip", "dir1/dir2", "")
	if err != nil {
		t.Fatalf("List(dir1/dir2): %v", err)
	}
	byName := entryNames(t, entries)
	if len(byName) != 1 {
		t.Fatalf("List(dir1/dir2) = %v, want 1 entry (c.txt)", byName)
	}
	c, ok := byName["c.txt"]
	if !ok || c.IsDir || c.Size != int64(len("nested-c")) {
		t.Fatalf("c.txt entry = %+v, ok=%v", c, ok)
	}
}

func TestListEmptyDirectory(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	entries, err := List(context.Background(), reader, mount+"/archive.zip", "emptydir", "")
	if err != nil {
		t.Fatalf("List(emptydir): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List(emptydir) = %v, want an empty (but non-error) listing", entries)
	}
}

func TestListNoSuchInnerPath(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, err := List(context.Background(), reader, mount+"/archive.zip", "does/not/exist", "")
	if !errors.Is(err, ErrInnerNotFound) {
		t.Fatalf("List(does/not/exist) error = %v, want ErrInnerNotFound", err)
	}
}

func TestListTraversalInnerPathRejected(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, err := List(context.Background(), reader, mount+"/archive.zip", "../etc", "")
	if !errors.Is(err, ErrInnerNotFound) {
		t.Fatalf("List(../etc) error = %v, want ErrInnerNotFound (no oracle on traversal attempts)", err)
	}
}

// ---- Extract ----

func drainAndClose(t *testing.T, rc interface {
	Read([]byte) (int, error)
	Close() error
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	tmp := make([]byte, 512)
	for {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractExactBytes(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	rc, size, err := Extract(context.Background(), reader, mount+"/archive.zip", "dir1/dir2/c.txt", "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if size != int64(len("nested-c")) {
		t.Fatalf("size = %d, want %d", size, len("nested-c"))
	}
	got := drainAndClose(t, rc)
	if string(got) != "nested-c" {
		t.Fatalf("Extract bytes = %q, want %q", got, "nested-c")
	}
}

func TestExtractOnDirectoryIsErrIsDir(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, _, err := Extract(context.Background(), reader, mount+"/archive.zip", "emptydir", "")
	if !errors.Is(err, ErrIsDir) {
		t.Fatalf("Extract(emptydir) error = %v, want ErrIsDir", err)
	}
}

func TestExtractInnerNotFound(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, _, err := Extract(context.Background(), reader, mount+"/archive.zip", "does/not/exist.txt", "")
	if !errors.Is(err, ErrInnerNotFound) {
		t.Fatalf("Extract(missing) error = %v, want ErrInnerNotFound", err)
	}
}

func TestExtractEncryptedEntryRefused(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	_, _, err := Extract(context.Background(), reader, mount+"/archive.zip", "secret.bin", "")
	if !errors.Is(err, ErrArchiveEncrypted) {
		t.Fatalf("Extract(secret.bin) error = %v, want ErrArchiveEncrypted", err)
	}
}

// TestExtractZipSlipEntryUnaddressable pins the zip-slip defense on the
// Extract path specifically: neither the raw traversal path nor its
// naively-normalized form ever resolves to the "../evil.txt" entry's bytes.
func TestExtractZipSlipEntryUnaddressable(t *testing.T) {
	reader, mount, _ := setupArchiveFixture(t)
	if _, _, err := Extract(context.Background(), reader, mount+"/archive.zip", "../evil.txt", ""); !errors.Is(err, ErrInnerNotFound) {
		t.Fatalf("Extract(../evil.txt) error = %v, want ErrInnerNotFound", err)
	}
	if _, _, err := Extract(context.Background(), reader, mount+"/archive.zip", "evil.txt", ""); !errors.Is(err, ErrInnerNotFound) {
		t.Fatalf("Extract(evil.txt) error = %v, want ErrInnerNotFound (the slip entry must be wholly unaddressable)", err)
	}
}

func TestExtractUnauthorizedDenied(t *testing.T) {
	_, mount, _ := setupArchiveFixture(t)
	noAccess := scopedNoAccessReader(80600)
	_, _, err := Extract(context.Background(), noAccess, mount+"/archive.zip", "a.txt", "")
	if !errors.Is(err, errs.PermissionDenied) {
		t.Fatalf("Extract unauthorized error = %v, want errs.PermissionDenied", err)
	}
}

// ---- Caps ----

func TestMaxZipEntriesCapRefuses(t *testing.T) {
	old := maxZipEntries
	maxZipEntries = 3
	t.Cleanup(func() { maxZipEntries = old })

	reader, mount, _ := setupArchiveFixture(t) // fixture has 6 raw entries, well above the lowered cap
	_, err := Meta(context.Background(), reader, mount+"/archive.zip", "")
	if !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("Meta over the lowered entry cap error = %v, want ErrTooManyEntries", err)
	}
}

func TestArchiveTotalSizeCapRefuses(t *testing.T) {
	old := maxZipTotalUncompressedSize
	maxZipTotalUncompressedSize = 2 // smaller than any single fixture file
	t.Cleanup(func() { maxZipTotalUncompressedSize = old })

	reader, mount, _ := setupArchiveFixture(t)
	_, err := Meta(context.Background(), reader, mount+"/archive.zip", "")
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("Meta over the lowered total-size cap error = %v, want ErrArchiveTooLarge", err)
	}
}

func TestExtractOversizeEntryRefused(t *testing.T) {
	old := maxExtractEntryBytes
	maxExtractEntryBytes = 4 // smaller than "nested-c" (8 bytes)
	t.Cleanup(func() { maxExtractEntryBytes = old })

	reader, mount, _ := setupArchiveFixture(t)
	_, _, err := Extract(context.Background(), reader, mount+"/archive.zip", "dir1/dir2/c.txt", "")
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("Extract over the lowered per-entry cap error = %v, want ErrEntryTooLarge", err)
	}
}

// ---- normalizeZipName unit tests ----

func TestNormalizeZipName(t *testing.T) {
	cases := []struct {
		in        string
		wantName  string
		wantIsDir bool
		wantOK    bool
	}{
		{"", "", false, true},
		{"a.txt", "a.txt", false, true},
		{"dir/", "dir", true, true},
		{"dir/sub/file.txt", "dir/sub/file.txt", false, true},
		{"./a.txt", "a.txt", false, true},
		{"a//b.txt", "a/b.txt", false, true},
		{"../evil", "", false, false},
		{"a/../b", "", false, false},
		{"..", "", false, false},
	}
	for _, c := range cases {
		name, isDir, ok := normalizeZipName(c.in)
		if name != c.wantName || isDir != c.wantIsDir || ok != c.wantOK {
			t.Errorf("normalizeZipName(%q) = (%q, %v, %v), want (%q, %v, %v)",
				c.in, name, isDir, ok, c.wantName, c.wantIsDir, c.wantOK)
		}
	}
}
