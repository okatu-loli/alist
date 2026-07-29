package op_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// 前置：init（storage_test.go 已有）建好内存库；此处挂一个 Local 存储在 /res 供解析
func setupResolveStorage(t *testing.T) uint {
	t.Helper()
	id, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/res",
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + t.TempDir() + `"}`,
	})
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	return id
}

func TestResolvePathAnchor(t *testing.T) {
	sid := setupResolveStorage(t)
	ref, err := op.ResolvePathAnchor("/res/photos/2026")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ref.StorageID != sid || ref.Anchor != "/photos/2026" || ref.AnchorKind != model.AnchorRelPath {
		t.Fatalf("bad ref: %+v", ref)
	}
}

func TestResolvePathAnchorMountRoot(t *testing.T) {
	sid := setupResolveStorage(t)
	ref, err := op.ResolvePathAnchor("/res")
	if err != nil {
		t.Fatalf("resolve mount root: %v", err)
	}
	if ref.StorageID != sid || ref.Anchor != "/" {
		t.Fatalf("bad root ref: %+v", ref)
	}
}

func TestResolvePathAnchorVirtualPath(t *testing.T) {
	setupResolveStorage(t)
	_, err := op.ResolvePathAnchor("/") // 挂载点之上的虚拟路径
	if !errors.Is(err, errs.StorageNotFound) {
		t.Fatalf("expect StorageNotFound for virtual path, got %v", err)
	}
}

// balancedObjIDAddition is the empty Additional for balancedObjIDDriver.
type balancedObjIDAddition struct{}

// balancedObjIDDriver is a driver.Driver whose sole listed object reports an
// ID derived from its OWN storage ID (obj-<storageID>), so a test can tell
// which member of a balance group actually served a given resolution.
type balancedObjIDDriver struct {
	model.Storage
}

func (d *balancedObjIDDriver) Config() driver.Config {
	return driver.Config{Name: "BalancedObjID", NoCache: true}
}

func (d *balancedObjIDDriver) GetAddition() driver.Additional {
	return &balancedObjIDAddition{}
}

func (d *balancedObjIDDriver) Init(ctx context.Context) error { return nil }

func (d *balancedObjIDDriver) Drop(ctx context.Context) error { return nil }

func (d *balancedObjIDDriver) GetRoot(ctx context.Context) (model.Obj, error) {
	return &model.Object{IsFolder: true, Name: "root"}, nil
}

func (d *balancedObjIDDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	id := fmt.Sprintf("obj-%d", d.GetStorage().ID)
	return []model.Obj{&model.Object{ID: id, Name: "doc.txt"}}, nil
}

func (d *balancedObjIDDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, errors.New("balancedObjIDDriver: Link not implemented")
}

func init() {
	op.RegisterDriver(func() driver.Driver { return &balancedObjIDDriver{} })
}

// setupBalanceGroupStorages mounts two instances of the same driver at
// "/bal" and "/bal.balance2": GetBalancedStorage round-robins between them
// on every call (internal/op/storage.go), so a resolver that resolves the
// same virtual path twice may hit a different storage each time.
func setupBalanceGroupStorages(t *testing.T) {
	t.Helper()
	for _, mp := range []string{"/bal", "/bal.balance2"} {
		id, err := op.CreateStorage(context.Background(), model.Storage{
			MountPath: mp,
			Driver:    "BalancedObjID",
			Addition:  "{}",
		})
		if err != nil {
			t.Fatalf("create storage %s: %v", mp, err)
		}
		t.Cleanup(func() { _ = op.DeleteStorageById(context.Background(), id) })
	}
}

// TestResolvePathAnchorBalanceGroupStableStorageID is the regression test for
// anchor storage_id normalization: GetBalancedStorage round-robins between
// "/bal" and "/bal.balance2" on every call, so ResolvePathAnchor must record
// the group's PRIMARY member ("/bal", the bare mount path) on every call —
// not whichever member happened to be picked that time — or a meta anchored
// via one call becomes invisible to a relocation fired from another member.
func TestResolvePathAnchorBalanceGroupStableStorageID(t *testing.T) {
	setupBalanceGroupStorages(t)
	primary, err := op.GetStorageByMountPath("/bal")
	if err != nil {
		t.Fatalf("get primary storage: %v", err)
	}
	primaryID := primary.GetStorage().ID
	for i := 0; i < 6; i++ {
		ref, err := op.ResolvePathAnchor("/bal/doc.txt")
		if err != nil {
			t.Fatalf("resolve iteration %d: %v", i, err)
		}
		if ref.StorageID != primaryID {
			t.Fatalf("iteration %d: ref.StorageID = %d, want primary member's ID %d (ref=%+v)",
				i, ref.StorageID, primaryID, ref)
		}
	}
}
