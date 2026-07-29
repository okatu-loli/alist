package fs

import (
	"context"
	"errors"
	"testing"

	// Import the Local driver directly (not the drivers aggregator package):
	// drivers/alias and drivers/crypt import internal/fs, so blank-importing
	// the full "drivers" package from *this* package's test file would create
	// an import cycle (fs test -> drivers -> drivers/alias -> fs).
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/xhofe/tache"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:fs_move_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
	MoveTaskManager = tache.NewManager[*MoveTask]()
}

func setupMoveTestStorage(t *testing.T, mountPath string) {
	t.Helper()
	root := t.TempDir()
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + root + `"}`,
	}); err != nil {
		t.Fatalf("create storage %s: %v", mountPath, err)
	}
	t.Cleanup(func() {
		s, err := op.GetStorageByMountPath(mountPath)
		if err == nil {
			_ = op.DeleteStorageById(context.Background(), s.GetStorage().ID)
		}
	})
}

// TestMoveAsTask_SameStorage_CreatesTaskInManager checks moveAsTask builds a
// MoveTask for a same-storage move and queues it in MoveTaskManager. Unlike
// copy, move has no synchronous same-storage shortcut: it always becomes a
// task so the caller gets an immediately-returned task to poll/cancel/retry.
func TestMoveAsTask_SameStorage_CreatesTaskInManager(t *testing.T) {
	setupMoveTestStorage(t, "/move-src-same")
	storage, actualRoot, err := op.GetStorageAndActualPath("/move-src-same")
	if err != nil {
		t.Fatalf("resolve storage: %v", err)
	}
	if err := op.MakeDir(context.Background(), storage, actualRoot+"/a"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := op.MakeDir(context.Background(), storage, actualRoot+"/b"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ctx := context.WithValue(context.Background(), ctxkey.User, &model.User{ID: 1, Username: "mover"})
	info, err := moveAsTask(ctx, "/move-src-same/a", "/move-src-same/b", false)
	if err != nil {
		t.Fatalf("moveAsTask: %v", err)
	}
	if info == nil {
		t.Fatal("moveAsTask returned nil task")
	}
	mt, ok := info.(*MoveTask)
	if !ok {
		t.Fatalf("moveAsTask returned %T, want *MoveTask", info)
	}
	if _, ok := MoveTaskManager.GetByID(mt.GetID()); !ok {
		t.Errorf("MoveTask %s was not added to MoveTaskManager", mt.GetID())
	}
	if mt.SrcStorageMp != "/move-src-same" || mt.DstStorageMp != "/move-src-same" {
		t.Errorf("unexpected storage mount paths: src=%q dst=%q", mt.SrcStorageMp, mt.DstStorageMp)
	}
}

// TestMoveAsTask_CrossStorage_ReturnsMoveBetweenTwoStorages checks moveAsTask
// rejects a cross-storage move up front (matching the old synchronous move's
// behavior) instead of silently queueing a task doomed to fail in Run().
func TestMoveAsTask_CrossStorage_ReturnsMoveBetweenTwoStorages(t *testing.T) {
	setupMoveTestStorage(t, "/move-src-cross")
	setupMoveTestStorage(t, "/move-dst-cross")

	ctx := context.WithValue(context.Background(), ctxkey.User, &model.User{ID: 1, Username: "mover"})
	_, err := moveAsTask(ctx, "/move-src-cross/whatever", "/move-dst-cross", false)
	if err == nil {
		t.Fatal("expected error for cross-storage move, got nil")
	}
	if !errors.Is(err, errs.MoveBetweenTwoStorages) {
		t.Errorf("got error %v, want errs.MoveBetweenTwoStorages", err)
	}
}
