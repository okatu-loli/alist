package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// fakeArchiveTool is a fully-controllable ext.ArchiveTool double for pinning
// the HOST-side dispatch layer's safety net (Phase E wave 2): every exported
// Meta/List/Extract funnels an ArchiveTool's return value through the SAME
// zip-slip/zip-bomb checks internal/archive's own zipArchiveTool is subject
// to, regardless of which tool produced it -- these tests prove that by
// having a fake, deliberately misbehaving tool try to smuggle a bad result
// past the host and confirming it never gets through.
type fakeArchiveTool struct {
	exts      []string
	metaFn    func() (ext.ArchiveToolMeta, error)
	listFn    func(innerPath string) ([]ext.ArchiveEntry, error)
	extractFn func(innerPath string) (io.ReadCloser, int64, error)
}

var _ ext.ArchiveTool = fakeArchiveTool{}

func (f fakeArchiveTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: "builtin.archive.testfake"} }
func (f fakeArchiveTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (f fakeArchiveTool) Shutdown(ctx context.Context) error         { return nil }
func (f fakeArchiveTool) Exts() []string                             { return f.exts }

func (f fakeArchiveTool) Meta(ctx context.Context, src ext.ArchiveSource, password string) (ext.ArchiveToolMeta, error) {
	return f.metaFn()
}

func (f fakeArchiveTool) List(ctx context.Context, src ext.ArchiveSource, innerPath, password string) ([]ext.ArchiveEntry, error) {
	return f.listFn(innerPath)
}

func (f fakeArchiveTool) Extract(ctx context.Context, src ext.ArchiveSource, innerPath, password string) (io.ReadCloser, int64, error) {
	return f.extractFn(innerPath)
}

// trackedCloser records whether Close was called, so a test can confirm the
// host's cap rejection actually released the tool's stream instead of
// leaking it.
type trackedCloser struct {
	io.Reader
	closed *bool
}

func (t *trackedCloser) Close() error {
	*t.closed = true
	return nil
}

var fakeArchiveFixtureSeq int64

// setupFakeArchiveFixture mounts an isolated Local storage containing a
// single file named data.fk (arbitrary bytes -- the fake tool never actually
// parses them) and registers tool under a fresh builtin.archive.testfake
// entry claiming the "fk" extension. Returns a caller scoped to ActRead and
// the file's full virtual path.
func setupFakeArchiveFixture(t *testing.T, tool ext.ArchiveTool) (reader *model.User, path string) {
	t.Helper()
	n := atomic.AddInt64(&fakeArchiveFixtureSeq, 1)
	mount := fmt.Sprintf("/fakearchive%d", n)

	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "data.fk"), []byte("irrelevant-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + rootDir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	entryID := fmt.Sprintf("builtin.archive.testfake%d", n)
	if err := extension.RegisterBuiltinExtension(extension.DefaultRegistry, ext.Manifest{
		ID:    entryID,
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}); err != nil {
		t.Fatal(err)
	}
	extension.RegisterTrustedArchiveToolInstance(entryID, tool)
	t.Cleanup(func() { extension.RegisterTrustedArchiveToolInstance(entryID, nil) })

	reader = &model.User{
		ID:       81000 + uint(n),
		Username: fmt.Sprintf("fakearchivereader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      mount,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}
	return reader, mount + "/data.fk"
}

// TestListExcludesUnsafeEntryNamesFromTool pins the host-side zip-slip net on
// a THIRD-PARTY tool's own output: a plugin returning a traversal-shaped
// ("../evil") or unexpectedly multi-segment ("nested/x") child name must have
// that entry silently excluded from the listing, exactly like the built-in
// zip tool already excludes its own zip-slip entries -- the host never
// trusts a tool to have already done this.
func TestListExcludesUnsafeEntryNamesFromTool(t *testing.T) {
	tool := fakeArchiveTool{
		exts: []string{"fk"},
		listFn: func(innerPath string) ([]ext.ArchiveEntry, error) {
			return []ext.ArchiveEntry{
				{Name: "good.txt", Size: 3},
				{Name: "../evil", Size: 4},
				{Name: "nested/x", Size: 5},
				{Name: "..", Size: 6},
				{Name: "", Size: 7},
			}, nil
		},
	}
	reader, path := setupFakeArchiveFixture(t, tool)

	entries, err := List(context.Background(), reader, path, "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "good.txt" {
		t.Fatalf("List = %+v, want only the single safe good.txt entry", entries)
	}
}

// TestMetaEntryCountCapAppliesToAnyTool pins the host-side zip-bomb net on
// Meta: a tool self-reporting an EntryCount over maxZipEntries is refused
// EXACTLY like an oversize built-in zip central directory, even though this
// fake tool never itself enforces the cap.
func TestMetaEntryCountCapAppliesToAnyTool(t *testing.T) {
	old := maxZipEntries
	maxZipEntries = 3
	t.Cleanup(func() { maxZipEntries = old })

	tool := fakeArchiveTool{
		exts:   []string{"fk"},
		metaFn: func() (ext.ArchiveToolMeta, error) { return ext.ArchiveToolMeta{EntryCount: 1000}, nil },
	}
	reader, path := setupFakeArchiveFixture(t, tool)

	if _, err := Meta(context.Background(), reader, path, ""); err == nil {
		t.Fatal("Meta over the entry-count cap = nil error, want ErrTooManyEntries")
	} else if err != ErrTooManyEntries {
		t.Fatalf("Meta over the entry-count cap error = %v, want ErrTooManyEntries", err)
	}
}

// TestMetaTotalSizeCapAppliesToAnyTool is TestMetaEntryCountCapAppliesToAnyTool's
// counterpart for the total-declared-uncompressed-size cap.
func TestMetaTotalSizeCapAppliesToAnyTool(t *testing.T) {
	old := maxZipTotalUncompressedSize
	maxZipTotalUncompressedSize = 10
	t.Cleanup(func() { maxZipTotalUncompressedSize = old })

	tool := fakeArchiveTool{
		exts:   []string{"fk"},
		metaFn: func() (ext.ArchiveToolMeta, error) { return ext.ArchiveToolMeta{TotalUncompressedSize: 1 << 30}, nil },
	}
	reader, path := setupFakeArchiveFixture(t, tool)

	if _, err := Meta(context.Background(), reader, path, ""); err == nil {
		t.Fatal("Meta over the total-size cap = nil error, want ErrArchiveTooLarge")
	} else if err != ErrArchiveTooLarge {
		t.Fatalf("Meta over the total-size cap error = %v, want ErrArchiveTooLarge", err)
	}
}

// TestExtractSizeCapAppliesToAnyToolAndClosesStream pins the host-side
// zip-bomb net on Extract: a tool self-reporting an entry size over
// maxExtractEntryBytes is refused before a single byte is read, AND the
// tool's own stream is Closed (no leaked resource) even though the fake tool
// never itself enforces the cap.
func TestExtractSizeCapAppliesToAnyToolAndClosesStream(t *testing.T) {
	old := maxExtractEntryBytes
	maxExtractEntryBytes = 4
	t.Cleanup(func() { maxExtractEntryBytes = old })

	closed := false
	tool := fakeArchiveTool{
		exts: []string{"fk"},
		extractFn: func(innerPath string) (io.ReadCloser, int64, error) {
			return &trackedCloser{Reader: bytes.NewReader([]byte("way too much data")), closed: &closed}, 1000, nil
		},
	}
	reader, path := setupFakeArchiveFixture(t, tool)

	if _, _, err := Extract(context.Background(), reader, path, "whatever.bin", ""); err == nil {
		t.Fatal("Extract over the per-entry cap = nil error, want ErrEntryTooLarge")
	} else if err != ErrEntryTooLarge {
		t.Fatalf("Extract over the per-entry cap error = %v, want ErrEntryTooLarge", err)
	}
	if !closed {
		t.Fatal("Extract must Close the tool's stream when refusing an oversize entry, not leak it")
	}
}

// TestExtractStreamsWithinCapFromTool is the happy-path counterpart: a tool
// reporting a size within the cap streams its bytes through untouched, and
// Close on the returned reader also closes the tool's own stream.
func TestExtractStreamsWithinCapFromTool(t *testing.T) {
	closed := false
	tool := fakeArchiveTool{
		exts: []string{"fk"},
		extractFn: func(innerPath string) (io.ReadCloser, int64, error) {
			return &trackedCloser{Reader: bytes.NewReader([]byte("hi")), closed: &closed}, 2, nil
		},
	}
	reader, path := setupFakeArchiveFixture(t, tool)

	rc, size, err := Extract(context.Background(), reader, path, "whatever.bin", "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if size != 2 {
		t.Fatalf("size = %d, want 2", size)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("bytes = %q, want %q", got, "hi")
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("Close on the returned reader must Close the tool's own stream too")
	}
}

// ---- remote (out-of-process / localize) dispatch path ----
//
// The tests above all register their fake tool as a trusted IN-PROCESS
// instance (RegisterTrustedArchiveToolInstance), so resolveArchiveSource
// always takes the ReaderAt fast path (remote=false) -- exactly like the
// built-in zip tool. The tests below instead register the fake tool behind
// extension.SetArchiveToolFactory on an ExecGRPC registry entry (mirroring
// how a real installed plugin resolves), forcing resolveArchiveSource down
// the localize-to-temp-file path (remote=true) that review fix 1/2 hardened:
// a size cap before any bytes are copied, and a cache so repeated Meta/List/
// Extract calls against the same archive reuse one localized file instead of
// re-downloading it every time.

// setupRemoteFakeArchiveFixture mounts an isolated Local storage containing a
// single file named "data.<extName>" and registers tool behind an ExecGRPC
// registry entry claiming extName, via extension.SetArchiveToolFactory --
// forcing resolveArchiveSource's remote=true (localize) path. extName must be
// unique per test (and distinct from setupFakeArchiveFixture's "fk") so
// concurrent/sequential tests in this package never contend over the same
// extension claim.
func setupRemoteFakeArchiveFixture(t *testing.T, tool ext.ArchiveTool, extName string, content []byte) (reader *model.User, path string) {
	t.Helper()
	n := atomic.AddInt64(&fakeArchiveFixtureSeq, 1)
	mount := fmt.Sprintf("/fakearchiveremote%d", n)

	rootDir := t.TempDir()
	fileName := "data." + extName
	if err := os.WriteFile(filepath.Join(rootDir, fileName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(context.Background(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + rootDir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), sid) })

	pluginID := fmt.Sprintf("com.example.testremotearchive%d", n)
	if err := extension.DefaultRegistry.RegisterPlugin(ext.Manifest{
		ID:    pluginID,
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}, "unused-binpath"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(pluginID) })

	extension.SetArchiveToolFactory(func(e *extension.Entry) (ext.ArchiveTool, func(), error) {
		if e.Manifest.ID != pluginID {
			return nil, nil, errors.New("not this test's plugin entry")
		}
		return tool, func() {}, nil
	})
	t.Cleanup(func() { extension.SetArchiveToolFactory(nil) })

	reader = &model.User{
		ID:       82000 + uint(n),
		Username: fmt.Sprintf("fakearchiveremotereader%d", n),
		Scoped:   true,
		ScopedPermissions: []model.Permission{{
			Path:      mount,
			Actions:   model.Actions{model.ActRead},
			Effect:    model.EffectAllow,
			Recursive: true,
		}},
	}
	return reader, mount + "/" + fileName
}

// TestResolveArchiveSourceLocalizeCapRefusesRemoteTool pins review fix 1: a
// remote (gRPC) ArchiveTool dispatch refuses to localize a source file larger
// than maxArchiveLocalizeBytes, before a single byte is downloaded.
func TestResolveArchiveSourceLocalizeCapRefusesRemoteTool(t *testing.T) {
	old := maxArchiveLocalizeBytes
	maxArchiveLocalizeBytes = 4 // smaller than the fixture content below
	t.Cleanup(func() { maxArchiveLocalizeBytes = old })

	tool := fakeArchiveTool{
		exts:   []string{"fx2"},
		metaFn: func() (ext.ArchiveToolMeta, error) { return ext.ArchiveToolMeta{}, nil },
	}
	reader, path := setupRemoteFakeArchiveFixture(t, tool, "fx2", []byte("this-is-well-over-four-bytes"))

	if _, err := Meta(context.Background(), reader, path, ""); err == nil {
		t.Fatal("Meta over the localize cap = nil error, want ErrArchiveLocalizeTooLarge")
	} else if !errors.Is(err, ErrArchiveLocalizeTooLarge) {
		t.Fatalf("Meta over the localize cap error = %v, want ErrArchiveLocalizeTooLarge", err)
	}
}

// TestResolveArchiveSourceReusesLocalizedFileAcrossCalls pins review fix 2's
// wiring at the dispatch layer: two independent resolveArchiveSource calls
// against the SAME archive (same path/size/modtime) must resolve to the
// exact same localized temp file, not a fresh download each time.
// localize_cache_test.go pins the cache's own download-counting/TTL/eviction
// behavior directly; this proves resolveArchiveSource actually routes through
// it end to end.
func TestResolveArchiveSourceReusesLocalizedFileAcrossCalls(t *testing.T) {
	tool := fakeArchiveTool{
		exts:   []string{"fx3"},
		metaFn: func() (ext.ArchiveToolMeta, error) { return ext.ArchiveToolMeta{EntryCount: 1}, nil },
	}
	reader, path := setupRemoteFakeArchiveFixture(t, tool, "fx3", []byte("remote-fixture-bytes"))

	_, src1, cleanup1, err := resolveArchiveSource(context.Background(), reader, path, "")
	if err != nil {
		t.Fatalf("resolveArchiveSource #1: %v", err)
	}
	_, src2, cleanup2, err := resolveArchiveSource(context.Background(), reader, path, "")
	if err != nil {
		t.Fatalf("resolveArchiveSource #2: %v", err)
	}
	if src1.Path == "" {
		t.Fatal("remote dispatch must populate ArchiveSource.Path")
	}
	if src1.Path != src2.Path {
		t.Fatalf("two calls against the same unchanged archive got different localized paths: %q vs %q, want reuse", src1.Path, src2.Path)
	}
	if _, err := os.Stat(src1.Path); err != nil {
		t.Fatalf("localized temp file must exist while referenced: %v", err)
	}
	cleanup1()
	cleanup2()
}

// TestExtractThroughRemoteToolInterruptedStreamStillReleasesCache pins review
// fix 4's "Extract stream interruption cleanup" case: closing Extract's
// returned reader WITHOUT draining it first (an interrupted download/preview)
// must still release the cache reference exactly like a full read would --
// proven by forcing the entry's TTL to "already expired" and confirming a
// later sweep (triggered by an unrelated localize call) actually removes the
// temp file, which only happens once its refcount is back at zero.
func TestExtractThroughRemoteToolInterruptedStreamStillReleasesCache(t *testing.T) {
	tool := fakeArchiveTool{
		exts: []string{"fx4"},
		extractFn: func(innerPath string) (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader([]byte("hello-remote-stream"))), 19, nil
		},
	}
	reader, path := setupRemoteFakeArchiveFixture(t, tool, "fx4", []byte("remote-fixture-bytes-for-extract"))

	rc, size, err := Extract(context.Background(), reader, path, "whatever.bin", "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if size != 19 {
		t.Fatalf("size = %d, want 19", size)
	}
	// Interrupt: close without reading a single byte.
	if err := rc.Close(); err != nil {
		t.Fatalf("Close (interrupted stream): %v", err)
	}

	// Force the entry to look already-expired -- mutating the entry's own
	// expires timestamp directly, NOT the global localizeCacheTTL var: TTL
	// only governs a NEW entry's (or a fresh cache HIT's) expiry, it does not
	// retroactively rewrite an already-inserted entry's stored expires.
	defaultLocalizeCache.mu.Lock()
	var tmpPath string
	for _, e := range defaultLocalizeCache.entries {
		if e.tmpPath != "" {
			tmpPath = e.tmpPath
			e.expires = time.Now().Add(-time.Hour)
		}
	}
	defaultLocalizeCache.mu.Unlock()
	if tmpPath == "" {
		t.Fatal("expected a localize-cache entry for the extracted archive")
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("localized temp file should still exist right after an interrupted Close (TTL not yet expired): %v", err)
	}

	// Trigger a sweep via an unrelated key -- this only actually removes the
	// file if the interrupted Close above really dropped the reference back
	// to zero.
	throwaway := filepath.Join(t.TempDir(), "throwaway")
	if err := os.WriteFile(throwaway, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, release, err := defaultLocalizeCache.localize(localizeCacheKey{path: "throwaway-key", size: 1, modified: 1}, func() (string, error) {
		return throwaway, nil
	}); err != nil {
		t.Fatalf("throwaway localize (sweep trigger): %v", err)
	} else {
		release()
	}

	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("localized temp file should have been swept once expired and unreferenced (interrupted Close must have released it), stat err = %v", err)
	}
}
