// This file exercises the nested-browsing regression directly at the op
// level: an ID-anchored plugin (ObjRef.Anchor is an opaque driver-internal
// ID, e.g. 123 Open's numeric fileID — the ext-contract default, no
// root_folder_path config field) must be able to list a subdirectory, not
// just the root. Before the fix, bridge.pluginDriver.GetRoot's fallback
// branch set both ID and Path on the root object; internal/op/fs.go's List
// "set path" step then stamped every child's Path as
// stdpath.Join(root.Path, child.Name) whenever the child came back with an
// empty Path (exactly what an ID-anchored plugin does), and refAnchor
// preferred that stamped Path over the child's real ID on every deeper call
// — an ID-anchored driver rejects that joined string as its anchor. This
// uses an in-process fake ext.StorageDriver (via a fake bridge.ProcManager)
// rather than a real gRPC subprocess, so it can make List's response depend
// on the anchor it's given — the real urlplugin/urldriver fixture shared by
// other tests always returns the same fixed single file regardless of
// anchor, which can't reproduce this bug.
package extension_test

import (
	"context"
	"encoding/json"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// idAnchoredNestedDriver is a synthetic ext.StorageDriver that serves a
// two-level tree keyed by ID-anchor (never Path), mirroring 123 Open:
// root ("0") contains one subdirectory ("42"/"tequilaOS"), which contains
// one file ("99"/"leaf.txt"). Any anchor it does not recognize (e.g. a
// path-joined string like "0/tequilaOS", which is what the pre-fix bug fed
// it) is rejected with ObjectNotFound, standing in for 123 Open's real
// strconv.ParseInt failure on a non-numeric anchor.
type idAnchoredNestedDriver struct{}

var (
	_ ext.StorageDriver = idAnchoredNestedDriver{}
)

func (idAnchoredNestedDriver) Manifest() ext.Manifest {
	return ext.Manifest{ID: "fake.nested.idanchored", Kinds: []ext.Kind{ext.KindStorageDriver}}
}
func (idAnchoredNestedDriver) Init(ctx context.Context, host ext.Host) error { return nil }
func (idAnchoredNestedDriver) Shutdown(ctx context.Context) error            { return nil }
func (idAnchoredNestedDriver) DriverConfig() ext.DriverConfig {
	return ext.DriverConfig{Name: "FakeNestedIDAnchored", DefaultRoot: "0"}
}
func (idAnchoredNestedDriver) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}
func (idAnchoredNestedDriver) DropInstance(ctx context.Context, storageID string) error { return nil }

func (idAnchoredNestedDriver) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	switch dir.Anchor {
	case "0": // root, by real fileID — not by any Path
		return []ext.Obj{{Name: "tequilaOS", IsDir: true, ID: "42"}}, nil
	case "42": // the subdirectory, addressed by its real fileID
		return []ext.Obj{{Name: "leaf.txt", Size: 7, ID: "99"}}, nil
	default:
		// Stands in for 123 Open's strconv.ParseInt failure on a non-numeric
		// anchor: reached only if something (incorrectly) fed this driver a
		// path-joined anchor like "0/tequilaOS" instead of "42".
		return nil, ext.NewError(ext.ErrCodeObjectNotFound, dir.Anchor)
	}
}

func (idAnchoredNestedDriver) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	return nil, ext.NewError(ext.ErrCodeNotImplement, "")
}

// fakeProcManager satisfies bridge.ProcManager by handing back an
// already-constructed in-process driver instead of launching a subprocess —
// this test only needs List's anchor-dependent behavior, not real gRPC
// transport.
type fakeProcManager struct{ d ext.StorageDriver }

func (f fakeProcManager) Acquire(ctx context.Context, pluginID, binPath string) (ext.StorageDriver, func(), error) {
	return f.d, func() {}, nil
}

// Restart satisfies bridge.ProcManager; this fake's in-process driver never
// crashes, so it just hands the same instance back.
func (f fakeProcManager) Restart(ctx context.Context, pluginID, binPath string, staleClient ext.StorageDriver) (ext.StorageDriver, error) {
	return f.d, nil
}

// TestPluginStorageNestedListWalksByID is the red/green regression test for
// the nested-browsing 500: before the GetRoot anchor fix, this fails with
// errs.ObjectNotFound (idAnchoredNestedDriver's stand-in for 123 Open's
// ParseInt panic) because op.List("/tequilaOS") ends up sending the
// path-joined anchor "0/tequilaOS" instead of the subdirectory's real fileID
// "42". After the fix, GetRoot's fallback root carries only ID ("0", no
// Path), so op/fs.go's List "set path" step never stamps a Path onto
// "tequilaOS", and refAnchor correctly falls back to its real ID for every
// deeper call.
func TestPluginStorageNestedListWalksByID(t *testing.T) {
	const entryID = "fake.nested.idanchored.entry"
	m := ext.Manifest{
		ID:         entryID,
		Name:       map[string]string{"en": "Fake ID-Anchored Nested"},
		Version:    "0.1.0",
		CoreAPI:    "^4.0.0",
		Kinds:      []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{Source: "official", License: "MIT"},
		// Deliberately no Info.Additional "root_folder_path" field, matching
		// 123 Open's manifest (which declares "root_folder_id" instead) — so
		// extension.IsPathAnchored(m) is false, same as the real factory wiring
		// in server/handles/plugins.go would compute for it.
	}
	// binPath must be non-empty for GetStorageDriver to treat this as a live
	// gRPC-mode entry (internal/extension/storage.go); fakeProcManager.Acquire
	// below ignores it entirely, so the value itself is a placeholder.
	if err := extension.DefaultRegistry.RegisterPluginMode(m, "fake-bin-path", extension.ExecGRPC); err != nil {
		t.Fatalf("RegisterPluginMode: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(entryID) })

	if extension.IsPathAnchored(m) {
		t.Fatal("test manifest must be id-anchored (no root_folder_path field) to reproduce the bug")
	}

	pm := fakeProcManager{d: idAnchoredNestedDriver{}}
	extension.SetPluginDriverFactory(func(e *extension.Entry) driver.Driver {
		return bridge.NewPluginDriver(e.ID(), e.BinPath(), e.Capabilities(), extension.IsPathAnchored(e.Manifest), pm)
	})
	t.Cleanup(func() { extension.SetPluginDriverFactory(nil) })

	ctx := context.Background()
	id, err := op.CreateStorage(ctx, model.Storage{Driver: entryID, MountPath: "/nested", Addition: "{}"})
	if err != nil {
		t.Fatalf("CreateStorage: %v", err)
	}
	t.Cleanup(func() {
		if err := op.DeleteStorageById(context.Background(), id); err != nil {
			t.Errorf("DeleteStorageById: %v", err)
		}
	})

	storageDriver, err := op.GetStorageByMountPath("/nested")
	if err != nil {
		t.Fatalf("GetStorageByMountPath: %v", err)
	}

	// Root listing: exercises the shallow path, unaffected either way.
	rootObjs, err := op.List(ctx, storageDriver, "/", model.ListArgs{})
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if len(rootObjs) != 1 || rootObjs[0].GetName() != "tequilaOS" || !rootObjs[0].IsDir() {
		t.Fatalf("List root = %+v, want one dir named tequilaOS", rootObjs)
	}

	// Nested listing: this is the regression. Pre-fix, op.List's "set path"
	// step stamps rootObjs[0]'s Path as "0/tequilaOS" during the root list
	// above (dir.GetPath()=="0" from the buggy GetRoot fallback, child Path
	// empty); refAnchor then sends that stamped string as the anchor here
	// instead of the real fileID "42", and idAnchoredNestedDriver rejects it.
	subObjs, err := op.List(ctx, storageDriver, "/tequilaOS", model.ListArgs{})
	if err != nil {
		t.Fatalf("List /tequilaOS: %v (this is the nested-browsing 500 regression)", err)
	}
	if len(subObjs) != 1 || subObjs[0].GetName() != "leaf.txt" || subObjs[0].GetSize() != 7 || subObjs[0].IsDir() {
		t.Fatalf("List /tequilaOS = %+v, want one file leaf.txt", subObjs)
	}
}
