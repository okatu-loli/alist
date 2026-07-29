package op

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

// createTestStorage mounts a Local driver storage at mount, rooted at dir,
// and cleans it up when the test ends. Mirrors the storage_test.go /
// resolve_test.go setup pattern (op_test package) but lives in package op so
// this file can reach unexported test helpers like resetObjRelocateHooks.
func createTestStorage(t *testing.T, mount, dir string) uint {
	t.Helper()
	id, err := CreateStorage(context.Background(), model.Storage{
		MountPath: mount,
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = DeleteStorageById(context.Background(), id) })
	return id
}

// resetObjRelocateHooks is a test helper: hooks are process-global.
func resetObjRelocateHooks() { objRelocateHooks = nil }

type relocateEvent struct {
	sid            uint
	kind           RelocateKind
	oldRel, newRel string
	isDir          bool
}

func TestRelocateHookFires(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid := createTestStorage(t, "/rel", dir)

	var events []relocateEvent
	RegisterObjRelocateHook(func(storageID uint, kind RelocateKind, oldRel, newRel string, isDir bool) {
		events = append(events, relocateEvent{storageID, kind, oldRel, newRel, isDir})
	})
	t.Cleanup(resetObjRelocateHooks)

	storage, _, err := GetStorageAndActualPath("/rel")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Rename(ctx, storage, "/a", "a2"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := Move(ctx, storage, "/f.txt", "/a2"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if err := Remove(ctx, storage, "/a2/f.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	want := []relocateEvent{
		{sid, RelocateRename, "/a", "/a2", true},
		{sid, RelocateMove, "/f.txt", "/a2/f.txt", false},
		{sid, RelocateRemove, "/a2/f.txt", "", false},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events %+v, want %d", len(events), events, len(want))
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}
