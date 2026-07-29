package extension

import (
	"context"
	"io"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// stubArchiveTool is a minimal ext.ArchiveTool for registry/dispatch tests.
// Meta/List/Extract are fixed, deterministic values -- exts is the only field
// tests vary.
type stubArchiveTool struct {
	id   string
	exts []string
	meta ext.ArchiveToolMeta
	list []ext.ArchiveEntry
}

func (s stubArchiveTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: s.id} }
func (s stubArchiveTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (s stubArchiveTool) Shutdown(ctx context.Context) error         { return nil }
func (s stubArchiveTool) Exts() []string                             { return s.exts }
func (s stubArchiveTool) Meta(ctx context.Context, src ext.ArchiveSource, password string) (ext.ArchiveToolMeta, error) {
	return s.meta, nil
}
func (s stubArchiveTool) List(ctx context.Context, src ext.ArchiveSource, innerPath, password string) ([]ext.ArchiveEntry, error) {
	return s.list, nil
}
func (s stubArchiveTool) Extract(ctx context.Context, src ext.ArchiveSource, innerPath, password string) (io.ReadCloser, int64, error) {
	return io.NopCloser(nil), 0, nil
}

func TestArchiveToolRegistry(t *testing.T) {
	// Clean any state from other tests in this package sharing the registry.
	for _, id := range ArchiveToolInstanceIDs() {
		RegisterTrustedArchiveToolInstance(id, nil)
	}

	fake := stubArchiveTool{id: "fake", exts: []string{"fk"}}
	if _, ok := GetArchiveTool("fake"); ok {
		t.Fatal("GetArchiveTool returned an instance before registration")
	}

	RegisterTrustedArchiveToolInstance("fake", fake)
	got, ok := GetArchiveTool("fake")
	if !ok || got.(stubArchiveTool).id != "fake" {
		t.Fatalf("GetArchiveTool(fake) = %v, %v; want the registered stub", got, ok)
	}
	ids := ArchiveToolInstanceIDs()
	if len(ids) != 1 || ids[0] != "fake" {
		t.Fatalf("ArchiveToolInstanceIDs() = %v, want [fake]", ids)
	}

	RegisterTrustedArchiveToolInstance("fake", nil)
	if _, ok := GetArchiveTool("fake"); ok {
		t.Fatal("GetArchiveTool still resolves after a nil re-registration (deregister)")
	}
}

// TestFindArchiveToolForExtInProcess pins the in-process resolution half of
// FindArchiveToolForExt: a trusted instance registered with an enabled owning
// Entry resolves with remote=false (the ReaderAt fast path is available);
// disabling the owning Entry makes the SAME extension resolve to nothing
// (mirrors offline_download's ToolByName enabled-gate), not silently fall
// back to some other claimant.
func TestFindArchiveToolForExtInProcess(t *testing.T) {
	r := NewRegistry()
	tool := stubArchiveTool{id: "builtin.archive.fake", exts: []string{".FK"}} // mixed case + leading dot on purpose
	if err := RegisterBuiltinExtension(r, ext.Manifest{
		ID:    "builtin.archive.fake",
		Kinds: []ext.Kind{ext.KindArchiveTool},
	}); err != nil {
		t.Fatalf("RegisterBuiltinExtension: %v", err)
	}
	RegisterTrustedArchiveToolInstance("builtin.archive.fake", tool)
	t.Cleanup(func() { RegisterTrustedArchiveToolInstance("builtin.archive.fake", nil) })

	got, remote, release, ok := FindArchiveToolForExt(r, "fk")
	if !ok || got == nil {
		t.Fatalf("FindArchiveToolForExt(fk) = %v, ok=%v; want the registered stub", got, ok)
	}
	if remote {
		t.Fatal("an in-process instance must resolve with remote=false")
	}
	release()
	// Case/dot-insensitivity: ".FK" (as declared) and "FK" (as looked up)
	// both normalize to the same "fk" slot.
	if _, _, _, ok := FindArchiveToolForExt(r, ".FK"); !ok {
		t.Fatal("FindArchiveToolForExt must be case/dot-insensitive")
	}

	if err := r.Disable("builtin.archive.fake"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, _, _, ok := FindArchiveToolForExt(r, "fk"); ok {
		t.Fatal("a disabled owning Entry must make FindArchiveToolForExt report not-found, not fall back silently")
	}
}

func TestFindArchiveToolForExtUnclaimedExtension(t *testing.T) {
	r := NewRegistry()
	if _, _, _, ok := FindArchiveToolForExt(r, "nope-nobody-claims-this"); ok {
		t.Fatal("an unclaimed extension must resolve to not-found")
	}
}
