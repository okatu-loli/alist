package op_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// --- fake drivers for op.PutRapid ---
//
// op.PutRapid takes a driver.Driver directly (it does NOT resolve a storage
// from a path), so these fakes exercise the wrapper's type-assert + delegation
// + cache behavior with no DB/mount at all.

// baseFakeDriver satisfies the minimum of driver.Driver (Meta + Reader) plus
// Getter/Mkdir so op.MakeDir + op.GetUnwrap resolve the destination dir obj
// (which op.PutRapid must pass to TryRapidUpload) without any real backend.
type baseFakeDriver struct {
	storage model.Storage
}

func (d *baseFakeDriver) Config() driver.Config {
	return driver.Config{Name: "FakeRapid"} // CheckStatus false -> no status gate
}
func (d *baseFakeDriver) GetStorage() *model.Storage     { return &d.storage }
func (d *baseFakeDriver) SetStorage(s model.Storage)     { d.storage = s }
func (d *baseFakeDriver) GetAddition() driver.Additional { return driver.RootID{RootFolderID: "0"} }
func (d *baseFakeDriver) Init(ctx context.Context) error { return nil }
func (d *baseFakeDriver) Drop(ctx context.Context) error { return nil }
func (d *baseFakeDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *baseFakeDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return &model.Link{}, nil
}

// Get makes every requested path resolve to an existing folder, so MakeDir is
// a no-op and GetUnwrap hands back the destination dir object.
func (d *baseFakeDriver) Get(ctx context.Context, path string) (model.Obj, error) {
	return &model.Object{ID: path, Path: path, Name: "dst", IsFolder: true}, nil
}

// rapidlessDriver deliberately does NOT implement driver.PutRapid.
type rapidlessDriver struct{ baseFakeDriver }

var _ driver.Driver = (*rapidlessDriver)(nil)

// rapidDriver implements driver.PutRapid with configurable outcome, and records
// the arguments it was delegated so the test can prove op.PutRapid forwarded
// the destination dir, name, size, and hashes verbatim.
type rapidDriver struct {
	baseFakeDriver
	hit     bool
	retErr  error
	gotDir  model.Obj
	gotName string
	gotSize int64
	gotHash utils.HashInfo
	called  bool
}

var _ driver.PutRapid = (*rapidDriver)(nil)

func (d *rapidDriver) TryRapidUpload(ctx context.Context, dstDir model.Obj, name string, size int64, hashes utils.HashInfo) (model.Obj, bool, error) {
	d.called = true
	d.gotDir, d.gotName, d.gotSize, d.gotHash = dstDir, name, size, hashes
	if d.retErr != nil {
		return nil, false, d.retErr
	}
	if !d.hit {
		return nil, false, nil
	}
	return &model.Object{ID: "new-file", Name: name, Size: size, HashInfo: hashes}, true, nil
}

func hashesMD5(v string) utils.HashInfo { return utils.NewHashInfo(utils.MD5, v) }

// TestPutRapidNotImplementedDriverReturnsNotImplement: a driver that does not
// implement PutRapid must yield errs.NotImplement, exactly like op.Put's
// default branch — the caller reads that as "fall back to streaming".
func TestPutRapidNotImplementedDriverReturnsNotImplement(t *testing.T) {
	d := &rapidlessDriver{}
	hit, err := op.PutRapid(context.Background(), d, "/dst", "file.bin", 10, hashesMD5("abc"))
	if hit {
		t.Fatal("a non-PutRapid driver must never report a rapid hit")
	}
	if !errors.Is(err, errs.NotImplement) {
		t.Fatalf("want errs.NotImplement, got %v", err)
	}
}

// TestPutRapidDelegatesAndReportsHit: a PutRapid driver that reports a hit makes
// op.PutRapid return (true, nil), and op.PutRapid must have forwarded name/size/
// hashes (and a non-nil resolved dstDir) unchanged.
func TestPutRapidDelegatesAndReportsHit(t *testing.T) {
	d := &rapidDriver{hit: true}
	hit, err := op.PutRapid(context.Background(), d, "/dst", "file.bin", 42, hashesMD5("deadbeef"))
	if err != nil {
		t.Fatalf("PutRapid: %v", err)
	}
	if !hit {
		t.Fatal("expected rapid hit")
	}
	if !d.called {
		t.Fatal("op.PutRapid must delegate to the driver's TryRapidUpload")
	}
	if d.gotName != "file.bin" || d.gotSize != 42 {
		t.Fatalf("delegated name/size = %q/%d, want file.bin/42", d.gotName, d.gotSize)
	}
	if d.gotHash.GetHash(utils.MD5) != "deadbeef" {
		t.Fatalf("delegated md5 = %q, want deadbeef", d.gotHash.GetHash(utils.MD5))
	}
	if d.gotDir == nil {
		t.Fatal("op.PutRapid must resolve and pass the destination dir obj")
	}
}

// TestPutRapidMissReturnsFalseNoError: a PutRapid driver reporting no match must
// make op.PutRapid return (false, nil) so the caller streams — a miss is not an
// error.
func TestPutRapidMissReturnsFalseNoError(t *testing.T) {
	d := &rapidDriver{hit: false}
	hit, err := op.PutRapid(context.Background(), d, "/dst", "file.bin", 10, hashesMD5("abc"))
	if err != nil {
		t.Fatalf("a miss must not be an error, got %v", err)
	}
	if hit {
		t.Fatal("expected miss")
	}
}

// TestPutRapidPropagatesDriverError: a driver error (e.g. errs.NotSupport for
// unusable hashes) is surfaced to the caller as (false, err) — the rapid-core
// is what decides an error means "fall back", not op.PutRapid.
func TestPutRapidPropagatesDriverError(t *testing.T) {
	d := &rapidDriver{retErr: errs.NotSupport}
	hit, err := op.PutRapid(context.Background(), d, "/dst", "file.bin", 10, hashesMD5("abc"))
	if hit {
		t.Fatal("an errored rapid attempt must not report a hit")
	}
	if !errors.Is(err, errs.NotSupport) {
		t.Fatalf("want errs.NotSupport surfaced, got %v", err)
	}
}
