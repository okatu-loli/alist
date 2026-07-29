package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/xhofe/tache"
)

// setupUploadTestTempDir points conf.Conf.TempDir at a fresh t.TempDir() for
// the duration of the test and restores the previous value afterward.
// conf.Conf is package-global test state seeded once by move_test.go's
// init(); its TempDir is an unusable relative path ("temp") since tests
// never run bootstrap.InitConfig(), so any test that touches temp files
// (uploads do, copy/move between Local storages don't) must set this up.
func setupUploadTestTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := conf.Conf.TempDir
	conf.Conf.TempDir = dir
	t.Cleanup(func() { conf.Conf.TempDir = old })
	return dir
}

func setupUploadTestStorage(t *testing.T, mountPath string) string {
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
		if s, err := op.GetStorageByMountPath(mountPath); err == nil {
			_ = op.DeleteStorageById(context.Background(), s.GetStorage().ID)
		}
	})
	return root
}

// TestUploadTask_JSONRoundTrip checks the persistable metadata fields
// (everything Run() needs to resume: destination storage/path, file
// name/size/mtime/mimetype, and the cached temp file's path) survive a
// persist/recover cycle, while the live storage/file handles -- unexported,
// so implicitly excluded from JSON -- do not.
func TestUploadTask_JSONRoundTrip(t *testing.T) {
	original := &UploadTask{
		DstStorageMp:     "/dst",
		DstDirActualPath: "/dir",
		FileName:         "f.txt",
		FileSize:         42,
		FileModified:     time.Unix(1000, 0).UTC(),
		FileMimetype:     "text/plain",
		TempFilePath:     "/tmp/upload-persist/file-abc",
	}
	original.SetID("upload-task-1")
	original.SetState(tache.StateRunning)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored UploadTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.DstStorageMp != original.DstStorageMp ||
		restored.DstDirActualPath != original.DstDirActualPath ||
		restored.FileName != original.FileName ||
		restored.FileSize != original.FileSize ||
		!restored.FileModified.Equal(original.FileModified) ||
		restored.FileMimetype != original.FileMimetype ||
		restored.TempFilePath != original.TempFilePath {
		t.Fatalf("persistable fields did not round-trip: got %+v", &restored)
	}
	if restored.GetID() != "upload-task-1" || restored.GetState() != tache.StateRunning {
		t.Errorf("tache.Base fields did not round-trip: id=%q state=%v", restored.GetID(), restored.GetState())
	}
	if restored.storage != nil || restored.file != nil {
		t.Errorf("live storage/file handles must not round-trip through JSON")
	}
	// GetNameArgs must work off the persisted fields alone -- a recovered
	// task can be listed before Run() ever populates storage/file.
	args := restored.GetNameArgs()
	if len(args) != 6 || args[1] != "f.txt" || args[3] != "/dst" || args[5] != "/dir" {
		t.Errorf("GetNameArgs() on an unrun recovered task = %v", args)
	}
}

// TestPutAsTask_PersistsCacheFileWhenEnabled proves the write side end to
// end: with TaskConfig.Upload.TaskPersistant enabled, putAsTask relocates
// the already-cached upload content into <TempDir>/upload-persist (so a
// crash before completion could resume it), the task still runs and lands
// the file at the destination, and the temp file is deleted once the task
// finishes (the same op.Put-closes-the-stream cleanup as before persistence
// existed -- this must not leak files into a directory CleanTempDir skips).
func TestPutAsTask_PersistsCacheFileWhenEnabled(t *testing.T) {
	tempDir := setupUploadTestTempDir(t)
	dstRoot := setupUploadTestStorage(t, "/upload-persist-dst")

	oldManager := UploadTaskManager
	UploadTaskManager = tache.NewManager[*UploadTask](tache.WithPersistDebounce(20 * time.Millisecond))
	t.Cleanup(func() { UploadTaskManager = oldManager })

	oldPersistant := conf.Conf.Tasks.Upload.TaskPersistant
	conf.Conf.Tasks.Upload.TaskPersistant = true
	t.Cleanup(func() { conf.Conf.Tasks.Upload.TaskPersistant = oldPersistant })

	content := "resumable upload content"
	s := &stream.FileStream{
		Obj: &model.Object{
			Name: "hello.txt",
			Size: int64(len(content)),
		},
		Reader:       strings.NewReader(content),
		Mimetype:     "text/plain",
		WebPutAsTask: true, // forces NeedStore()==true, matching a real As-Task upload
	}

	ctx := context.WithValue(context.Background(), ctxkey.User, &model.User{ID: 1, Username: "uploader"})
	info, err := putAsTask(ctx, "/upload-persist-dst", s)
	if err != nil {
		t.Fatalf("putAsTask: %v", err)
	}
	ut, ok := info.(*UploadTask)
	if !ok {
		t.Fatalf("putAsTask returned %T, want *UploadTask", info)
	}
	if ut.TempFilePath == "" {
		t.Fatal("TempFilePath was not set even though TaskPersistant is enabled")
	}
	wantDir := filepath.Join(tempDir, UploadPersistTempDirName)
	if filepath.Dir(ut.TempFilePath) != wantDir {
		t.Errorf("TempFilePath = %q, want it under %q", ut.TempFilePath, wantDir)
	}
	if _, err := os.Stat(ut.TempFilePath); err != nil {
		t.Fatalf("relocated temp file missing right after putAsTask: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := ut.GetState(); s == tache.StateSucceeded || s == tache.StateFailed || s == tache.StateErrored {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ut.GetState() != tache.StateSucceeded {
		t.Fatalf("upload ended in state %v (err=%v), want StateSucceeded", ut.GetState(), ut.GetErr())
	}

	got, err := os.ReadFile(filepath.Join(dstRoot, "hello.txt"))
	if err != nil || string(got) != content {
		t.Fatalf("uploaded content = %q, err=%v, want %q", got, err, content)
	}

	if _, err := os.Stat(ut.TempFilePath); !os.IsNotExist(err) {
		t.Errorf("temp file %q should have been deleted after the task finished, stat err=%v", ut.TempFilePath, err)
	}
}

// TestUploadTaskManager_ResumesUnfinishedTaskFromPersistedDB proves the read
// (resume) side: an UploadTask persisted mid-flight, whose temp file is
// still sitting on disk, is loaded and re-run -- reopening TempFilePath and
// re-resolving the destination storage -- when a new manager is built the
// way bootstrap.InitTaskManager builds it.
func TestUploadTaskManager_ResumesUnfinishedTaskFromPersistedDB(t *testing.T) {
	tempDir := setupUploadTestTempDir(t)
	dstRoot := setupUploadTestStorage(t, "/upload-resume-dst")

	persistDir := filepath.Join(tempDir, UploadPersistTempDirName)
	if err := os.MkdirAll(persistDir, 0o777); err != nil {
		t.Fatalf("mkdir persist dir: %v", err)
	}
	tempFilePath := filepath.Join(persistDir, "file-resume-1")
	content := "resume me from disk"
	if err := os.WriteFile(tempFilePath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}

	const typeKey = "upload-resume-test"
	oldManager := UploadTaskManager
	t.Cleanup(func() { UploadTaskManager = oldManager })

	unfinished := &UploadTask{
		DstStorageMp:     "/upload-resume-dst",
		DstDirActualPath: "/",
		FileName:         "resumed.txt",
		FileSize:         int64(len(content)),
		FileMimetype:     "text/plain",
		TempFilePath:     tempFilePath,
	}
	unfinished.SetID("resume-upload-1")
	unfinished.SetState(tache.StateRunning)

	persisted, err := json.Marshal([]*UploadTask{unfinished})
	if err != nil {
		t.Fatalf("marshal persisted tasks: %v", err)
	}
	if err := db.CreateTaskData(&model.TaskItem{Key: typeKey, PersistData: string(persisted)}); err != nil {
		t.Fatalf("seed persisted task row: %v", err)
	}
	t.Cleanup(func() {
		_ = db.UpdateTaskData(&model.TaskItem{Key: typeKey, PersistData: "[]"})
	})

	UploadTaskManager = tache.NewManager[*UploadTask](
		tache.WithPersistFunction(db.GetTaskDataFunc(typeKey, true), db.UpdateTaskDataFunc(typeKey, true)),
		tache.WithPersistDebounce(20*time.Millisecond),
	)

	got, ok := UploadTaskManager.GetByID("resume-upload-1")
	if !ok {
		t.Fatal("unfinished upload task was not loaded into the rebuilt manager")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := got.GetState(); s == tache.StateSucceeded || s == tache.StateFailed || s == tache.StateErrored {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.GetState() != tache.StateSucceeded {
		t.Fatalf("resumed upload ended in state %v (err=%v), want StateSucceeded", got.GetState(), got.GetErr())
	}

	gotBytes, err := os.ReadFile(filepath.Join(dstRoot, "resumed.txt"))
	if err != nil || string(gotBytes) != content {
		t.Fatalf("resumed upload did not land at destination: content=%q err=%v", gotBytes, err)
	}
	if _, err := os.Stat(tempFilePath); !os.IsNotExist(err) {
		t.Errorf("resumed task's temp file should be deleted after completion, stat err=%v", err)
	}
}

// TestReconcileUploadPersistTempDir_RemovesOrphans checks the sweep that
// keeps <TempDir>/upload-persist from growing unbounded: files not
// referenced by any task currently loaded in UploadTaskManager (e.g. left
// behind by a task canceled/removed before it ran) get deleted, while files
// referenced by a loaded task are kept.
func TestReconcileUploadPersistTempDir_RemovesOrphans(t *testing.T) {
	tempDir := setupUploadTestTempDir(t)
	persistDir := filepath.Join(tempDir, UploadPersistTempDirName)
	if err := os.MkdirAll(persistDir, 0o777); err != nil {
		t.Fatalf("mkdir persist dir: %v", err)
	}
	keepPath := filepath.Join(persistDir, "keep-me")
	orphanPath := filepath.Join(persistDir, "orphan")
	if err := os.WriteFile(keepPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("seed keep file: %v", err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("seed orphan file: %v", err)
	}

	oldManager := UploadTaskManager
	// A manager with no persist function: Add() below just registers the
	// task in memory (as recover() would have, without touching the DB).
	UploadTaskManager = tache.NewManager[*UploadTask]()
	t.Cleanup(func() { UploadTaskManager = oldManager })
	kept := &UploadTask{TempFilePath: keepPath}
	kept.SetState(tache.StatePending)
	UploadTaskManager.Add(kept)

	ReconcileUploadPersistTempDir()

	if _, err := os.Stat(keepPath); err != nil {
		t.Errorf("referenced temp file was removed by reconcile: %v", err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphaned temp file was not removed by reconcile, stat err=%v", err)
	}
}

// TestReconcileUploadPersistTempDir_NilManagerDoesNotPanic checks the
// startup ordering guard: if this ever ran before UploadTaskManager is
// constructed, it must treat "nothing to keep" rather than panic.
func TestReconcileUploadPersistTempDir_NilManagerDoesNotPanic(t *testing.T) {
	tempDir := setupUploadTestTempDir(t)
	persistDir := filepath.Join(tempDir, UploadPersistTempDirName)
	if err := os.MkdirAll(persistDir, 0o777); err != nil {
		t.Fatalf("mkdir persist dir: %v", err)
	}
	orphanPath := filepath.Join(persistDir, "orphan")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("seed orphan file: %v", err)
	}

	oldManager := UploadTaskManager
	UploadTaskManager = nil
	t.Cleanup(func() { UploadTaskManager = oldManager })

	ReconcileUploadPersistTempDir() // must not panic

	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphan should still be swept with a nil manager, stat err=%v", err)
	}
}
