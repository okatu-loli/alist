package webdav

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:webdav_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
	// Production seeds the guest/admin roles at the reserved IDs
	// model.GUEST(1)/model.ADMIN(2) via bootstrap/data.InitData before any
	// custom role is ever created (internal/bootstrap/data/role.go). This
	// test package runs only the migration, not that seed step, so without
	// this the FIRST role a test creates would land on autoincrement ID 1 --
	// colliding with the literal GUEST sentinel model.User.IsGuest() checks
	// (u.Role.Contains(model.GUEST)) and making every test user spuriously
	// "a guest". Burn those two IDs on throwaway roles up front so
	// grantRole's real fixtures always start at ID 3+, matching production.
	for i := 0; i < 2; i++ {
		if err := op.CreateRole(&model.Role{Name: fmt.Sprintf("dav-id-burn-%d", i)}); err != nil {
			panic(err)
		}
	}
}

// setupTempDir points conf.Conf.TempDir at a fresh t.TempDir() -- required
// for openForWrite's os.CreateTemp spool (conf.DefaultConfig's TempDir is an
// unusable relative path in a test binary that never runs
// bootstrap.InitConfig, same reasoning as internal/fs/put_test.go's
// setupUploadTestTempDir).
func setupTempDir(t *testing.T) {
	t.Helper()
	old := conf.Conf.TempDir
	conf.Conf.TempDir = t.TempDir()
	t.Cleanup(func() { conf.Conf.TempDir = old })
}

// setupLocalStorage registers a writable Local storage at mountPath backed
// by a fresh temp directory, cleaned up automatically.
func setupLocalStorage(t *testing.T, mountPath string) string {
	t.Helper()
	root := t.TempDir()
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + root + `"}`,
	}); err != nil {
		t.Fatalf("create local storage %s: %v", mountPath, err)
	}
	t.Cleanup(func() {
		if s, err := op.GetStorageByMountPath(mountPath); err == nil {
			_ = op.DeleteStorageById(context.Background(), s.GetStorage().ID)
		}
	})
	return root
}

var testUserSeq int64

// newTestUser creates and persists a plain (non-admin, non-scoped) user with
// the given raw primary password and 2FA state.
func newTestUser(t *testing.T, password string, otpSecret string) *model.User {
	t.Helper()
	n := atomic.AddInt64(&testUserSeq, 1)
	u := &model.User{
		Username:  fmt.Sprintf("davuser%d", n),
		OtpSecret: otpSecret,
	}
	u.SetPassword(password)
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteUserById(u.ID) })
	return u
}

// grantFullAccess gives user a role with unrestricted read/write/rename/
// move/copy/remove on path (recursive), mirroring internal/op/authz_test.go's
// makeAuthzUser helper -- this package can't reach that unexported helper
// (different package, _test.go files aren't importable), so it's
// reimplemented here against the same public op/db surface.
func grantFullAccess(t *testing.T, user *model.User, path string) {
	t.Helper()
	grantRole(t, user, path, model.Actions{
		model.ActRead, model.ActWrite, model.ActRename,
		model.ActMove, model.ActCopy, model.ActRemove,
	})
}

// grantReadOnlyAccess gives user a role with ONLY ActRead on path -- the
// "read-only-scope user" fixture the PUT/MKCOL/DELETE -> 403 tests need.
func grantReadOnlyAccess(t *testing.T, user *model.User, path string) {
	t.Helper()
	grantRole(t, user, path, model.Actions{model.ActRead})
}

func grantRole(t *testing.T, user *model.User, path string, actions model.Actions) {
	t.Helper()
	grantRoleWithEffect(t, user, path, actions, model.EffectAllow)
}

// denyReadAccess grants user an additional role that DENIES ActRead on path
// -- the per-child RBAC case op.FilterListing enforces (its Denied(ActRead)
// branch), used by the PROPFIND/Readdir leak test to prove a denied child is
// dropped from a listing rather than merely erroring on direct access.
func denyReadAccess(t *testing.T, user *model.User, path string) {
	t.Helper()
	grantRoleWithEffect(t, user, path, model.Actions{model.ActRead}, model.EffectDeny)
}

func grantRoleWithEffect(t *testing.T, user *model.User, path string, actions model.Actions, effect string) {
	t.Helper()
	n := atomic.AddInt64(&testUserSeq, 1)
	r := &model.Role{Name: fmt.Sprintf("dav-role-%d", n)}
	if err := op.CreateRole(r); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(r.ID) })
	p := &model.Permission{
		Name: fmt.Sprintf("dav-perm-%d", n), Path: path,
		Actions: actions, Effect: effect, Recursive: true,
	}
	if err := db.CreatePermission(p); err != nil {
		t.Fatalf("create permission: %v", err)
	}
	t.Cleanup(func() { _ = db.DeletePermissionById(p.ID) })
	if err := db.AddRolePermission(r.ID, p.ID); err != nil {
		t.Fatalf("add role permission: %v", err)
	}
	op.InvalidateRolePolicyForTest(r.ID)
	user.Role = append(user.Role, int(r.ID))
	if err := db.UpdateUser(user); err != nil {
		t.Fatalf("update user role: %v", err)
	}
}

// setMaxPutBytesForTest temporarily lowers maxWebdavPutBytes so a test can
// exercise the abuse-guard/truncation-abort path (Finding 3) without
// actually spooling a multi-gigabyte body.
func setMaxPutBytesForTest(t *testing.T, n int64) {
	t.Helper()
	old := maxWebdavPutBytes
	maxWebdavPutBytes = n
	t.Cleanup(func() { maxWebdavPutBytes = old })
}
