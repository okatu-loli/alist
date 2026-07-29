package bridge

import (
	"context"
	"testing"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// --- fake legacy drivers for StorageBridge.TryRapidUpload ---
//
// These mirror the shape of internal/op/fs_putrapid_test.go's rapidDriver /
// rapidlessDriver, but here they are handed to NewStorageBridge as a ctor so
// the bridge's own instance-resolution + PutRapid type-assert + error-recode
// path gets exercised (not op.PutRapid's).

type rapidBaseDriver struct {
	storage model.Storage
}

func (d *rapidBaseDriver) Config() driver.Config          { return driver.Config{Name: "FakeRapid"} }
func (d *rapidBaseDriver) GetStorage() *model.Storage     { return &d.storage }
func (d *rapidBaseDriver) SetStorage(s model.Storage)     { d.storage = s }
func (d *rapidBaseDriver) GetAddition() driver.Additional { return driver.RootID{RootFolderID: "0"} }
func (d *rapidBaseDriver) Init(ctx context.Context) error { return nil }
func (d *rapidBaseDriver) Drop(ctx context.Context) error { return nil }
func (d *rapidBaseDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *rapidBaseDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return &model.Link{}, nil
}

// rapidlessBridgeDriver deliberately does NOT implement driver.PutRapid.
type rapidlessBridgeDriver struct{ rapidBaseDriver }

var _ driver.Driver = (*rapidlessBridgeDriver)(nil)

// rapidBridgeDriver implements driver.PutRapid with a configurable outcome.
type rapidBridgeDriver struct {
	rapidBaseDriver
	hit    bool
	retErr error
}

var _ driver.PutRapid = (*rapidBridgeDriver)(nil)

func (d *rapidBridgeDriver) TryRapidUpload(ctx context.Context, dstDir model.Obj, name string, size int64, hashes utils.HashInfo) (model.Obj, bool, error) {
	if d.retErr != nil {
		return nil, false, d.retErr
	}
	if !d.hit {
		return nil, false, nil
	}
	return &model.Object{Name: name, ID: "999"}, true, nil
}

func newRapidBridge(t *testing.T, drv driver.Driver) *StorageBridge {
	t.Helper()
	b := NewStorageBridge(func() driver.Driver { return drv }, ext.Manifest{Name: map[string]string{"en": "FakeRapid"}})
	if err := b.NewInstance(context.Background(), "1", nil); err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = b.Shutdown(context.Background()) })
	return b
}

func TestStorageBridgeTryRapidUploadHit(t *testing.T) {
	b := newRapidBridge(t, &rapidBridgeDriver{hit: true})
	o, hit, err := b.TryRapidUpload(context.Background(), ext.ObjRef{StorageID: "1", Anchor: "/dst"}, "file.bin", 10, map[string]string{"md5": "d41d8cf98f00b204e9800998ecf8427e"})
	if err != nil {
		t.Fatalf("TryRapidUpload: %v", err)
	}
	if !hit {
		t.Fatal("expected hit")
	}
	if o.Name != "file.bin" || o.ID != "999" {
		t.Fatalf("obj = %+v, want Name=file.bin ID=999", o)
	}
}

// nilObjRapidBridgeDriver reports a hit but hands back no model.Obj at all —
// the shape a legacy driver.PutRapid takes when it only registers the file
// by hash and has nothing else to return.
type nilObjRapidBridgeDriver struct{ rapidBaseDriver }

var _ driver.PutRapid = (*nilObjRapidBridgeDriver)(nil)

func (d *nilObjRapidBridgeDriver) TryRapidUpload(ctx context.Context, dstDir model.Obj, name string, size int64, hashes utils.HashInfo) (model.Obj, bool, error) {
	return nil, true, nil
}

// TestStorageBridgeTryRapidUploadHitWithNilObj guards the M1 fix: a legacy
// PutRapid hit with a nil Obj must still be reported as a hit (Obj{}, true,
// nil), not silently downgraded to a miss. Downgrading it would make the
// caller stream the whole file again even though the rapid upload already
// succeeded server-side.
func TestStorageBridgeTryRapidUploadHitWithNilObj(t *testing.T) {
	b := newRapidBridge(t, &nilObjRapidBridgeDriver{})
	o, hit, err := b.TryRapidUpload(context.Background(), ext.ObjRef{StorageID: "1", Anchor: "/dst"}, "file.bin", 10, map[string]string{"md5": validMD5})
	if err != nil {
		t.Fatalf("TryRapidUpload: %v", err)
	}
	if !hit {
		t.Fatal("a PutRapid hit with a nil Obj must still be reported as a hit")
	}
	if o.Name != "" || o.ID != "" {
		t.Fatalf("expected zero Obj alongside the hit, got %+v", o)
	}
}

func TestStorageBridgeTryRapidUploadMiss(t *testing.T) {
	b := newRapidBridge(t, &rapidBridgeDriver{hit: false})
	o, hit, err := b.TryRapidUpload(context.Background(), ext.ObjRef{StorageID: "1", Anchor: "/dst"}, "file.bin", 10, map[string]string{"md5": "abc"})
	if err != nil {
		t.Fatalf("TryRapidUpload: %v", err)
	}
	if hit {
		t.Fatal("expected miss")
	}
	if o.Name != "" || o.ID != "" || o.Hashes != nil {
		t.Fatalf("expected zero Obj on miss, got %+v", o)
	}
}

func TestStorageBridgeTryRapidUploadNotSupport(t *testing.T) {
	b := newRapidBridge(t, &rapidBridgeDriver{retErr: errs.NotSupport})
	_, hit, err := b.TryRapidUpload(context.Background(), ext.ObjRef{StorageID: "1", Anchor: "/dst"}, "file.bin", 10, map[string]string{"md5": "abc"})
	if hit {
		t.Fatal("an errored rapid attempt must not report a hit")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotSupport {
		t.Fatalf("CodeOf(err) = %v, %v, want ErrCodeNotSupport", code, ok)
	}
}

func TestStorageBridgeTryRapidUploadNotImplemented(t *testing.T) {
	b := newRapidBridge(t, &rapidlessBridgeDriver{})
	_, hit, err := b.TryRapidUpload(context.Background(), ext.ObjRef{StorageID: "1", Anchor: "/dst"}, "file.bin", 10, map[string]string{"md5": "abc"})
	if hit {
		t.Fatal("a non-PutRapid driver must never report a rapid hit")
	}
	if err == nil {
		t.Fatal("expected error")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("CodeOf(err) = %v, %v, want ErrCodeNotImplement", code, ok)
	}
}

// --- hash mapping regression (both directions) ---

// validMD5 is a real 32-hex-char md5 digest (of the empty string), used
// wherever a test needs a hash value that survives hashesFromMap's digest
// width validation (see TestHashesFromMapRejectsWrongWidth) — unlike a
// placeholder like "abc", which is correctly dropped as undersized.
const validMD5 = "d41d8cf98f00b204e9800998ecf8427e"

func TestHashesToMapAndBack(t *testing.T) {
	hi := utils.NewHashInfo(utils.MD5, validMD5)
	m := hashesToMap(hi)
	if m["md5"] != validMD5 {
		t.Fatalf("hashesToMap = %+v, want md5=%s", m, validMD5)
	}
	back := hashesFromMap(map[string]string{"md5": validMD5})
	if back.GetHash(utils.MD5) != validMD5 {
		t.Fatalf("hashesFromMap round-trip md5 = %q, want %s", back.GetHash(utils.MD5), validMD5)
	}
}

func TestExtObjToModelCarriesHash(t *testing.T) {
	o := extObjToModel(ext.Obj{Hashes: map[string]string{"md5": validMD5}})
	if o.GetHash().GetHash(utils.MD5) != validMD5 {
		t.Fatalf("extObjToModel hash = %q, want %s", o.GetHash().GetHash(utils.MD5), validMD5)
	}
}

func TestObjToExtCarriesHash(t *testing.T) {
	o := &model.Object{HashInfo: utils.NewHashInfo(utils.MD5, "abc")}
	ex := objToExt(o)
	if ex.Hashes["md5"] != "abc" {
		t.Fatalf("objToExt hashes = %+v, want md5=abc", ex.Hashes)
	}
}

// TestHashesFromMapRejectsWrongWidth guards against a plugin (malicious or
// buggy) supplying a hash value whose length doesn't match its algorithm's
// registered digest width — e.g. a chunk/partial hash or a truncated
// digest passed off as a whole-file md5. utils.FromString only checks
// non-empty, so this filtering must happen in the bridge (see
// hashesFromMap's doc comment / ext.Obj.Hashes's contract doc).
func TestHashesFromMapRejectsWrongWidth(t *testing.T) {
	hi := hashesFromMap(map[string]string{"md5": "tooshort"})
	if got := hi.GetHash(utils.MD5); got != "" {
		t.Fatalf("hashesFromMap with undersized md5 = %q, want empty (dropped)", got)
	}

	hi = hashesFromMap(map[string]string{"md5": validMD5})
	if got := hi.GetHash(utils.MD5); got != validMD5 {
		t.Fatalf("hashesFromMap with valid-width md5 = %q, want %q (kept)", got, validMD5)
	}
}
