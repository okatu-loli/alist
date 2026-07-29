package op

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/db"
)

// TestUpdateStorageValidatesBeforePersist is a regression test for the
// residual persist-before-validate walkway left open after Step 5's fixes
// (storage_update_driver_check_test.go, storage_rename_scope_sync_test.go).
// Those fixes stopped a doomed "enable + rename" UpdateStorage call from
// corrupting role permission JSON, but db.UpdateStorage(&storage) itself
// still ran unconditionally before the driver-existence precondition was
// checked — so the storage ROW was permanently rewritten (new mount path,
// Disabled=false) in the database even though the call ultimately failed and
// no driver was ever reinitialized for it. A retry of the identical request
// would see the already-rewritten oldStorage and fail identically forever,
// leaving the database out of sync with reality (no live driver anywhere)
// with no way to recover except manual DB surgery.
//
// This test pins that db.UpdateStorage must not run until the driver
// precondition is confirmed: after a failed "re-enable + rename" call, the
// storage row in the database must be untouched (old mount path, still
// disabled). Counterfactual: reverting the precondition-hoist in
// UpdateStorage (moving the GetStorageByMountPath check back to after
// db.UpdateStorage, as it was before this fix) makes this test fail, because
// the row would show the new mount path and Disabled=false despite the
// error return.
func TestUpdateStorageValidatesBeforePersist(t *testing.T) {
	dir := t.TempDir()
	sid := createTestStorage(t, "/s8t5-old", dir)

	if err := DisableStorage(context.Background(), sid); err != nil {
		t.Fatalf("disable storage: %v", err)
	}

	storage, err := db.GetStorageById(sid)
	if err != nil {
		t.Fatalf("get storage: %v", err)
	}

	// Combined enable + rename against a storage whose driver was already
	// removed from storagesMap by DisableStorage above: there is no live
	// driver to Drop/reinit, so this call can never succeed.
	storage.Disabled = false
	storage.MountPath = "/s8t5-new"
	if err := UpdateStorage(context.Background(), *storage); err == nil {
		t.Fatalf("expected UpdateStorage to fail (no live driver for the disabled storage's old mount), got nil error")
	}

	persisted, err := db.GetStorageById(sid)
	if err != nil {
		t.Fatalf("get storage after failed update: %v", err)
	}
	if persisted.MountPath != "/s8t5-old" {
		t.Errorf("storage row was renamed despite UpdateStorage failing: MountPath=%q, want unchanged %q", persisted.MountPath, "/s8t5-old")
	}
	if !persisted.Disabled {
		t.Errorf("storage row was marked enabled despite UpdateStorage failing: Disabled=%v, want true (unchanged)", persisted.Disabled)
	}

	// A retry of the exact same request must fail the same clean way, not
	// succeed accidentally or corrupt further state, and not have already
	// half-applied itself via a prior attempt.
	storage2, err := db.GetStorageById(sid)
	if err != nil {
		t.Fatalf("get storage before retry: %v", err)
	}
	storage2.Disabled = false
	storage2.MountPath = "/s8t5-new"
	if err := UpdateStorage(context.Background(), *storage2); err == nil {
		t.Fatalf("expected retry of UpdateStorage to fail identically, got nil error")
	}
}
