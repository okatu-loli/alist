package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/xhofe/tache"
)

// TestCopyTask_JSONRoundTrip checks CopyTask survives a persist/recover cycle:
// every field tache needs to resume it (object paths + storage mount paths,
// id/state/retry from tache.Base) marshals and unmarshals without loss, while
// the live driver.Driver handles (srcStorage/dstStorage, unexported so
// implicitly excluded from JSON) are dropped — Run() re-resolves them from
// SrcStorageMp/DstStorageMp via op.GetStorageByMountPath.
func TestCopyTask_JSONRoundTrip(t *testing.T) {
	original := &CopyTask{
		SrcObjPath:   "/a/b.txt",
		DstDirPath:   "/c",
		SrcStorageMp: "/src",
		DstStorageMp: "/dst",
	}
	original.SetID("task-1")
	original.SetState(tache.StateRunning)
	original.SetRetry(1, 3)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored CopyTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.SrcObjPath != original.SrcObjPath || restored.DstDirPath != original.DstDirPath ||
		restored.SrcStorageMp != original.SrcStorageMp || restored.DstStorageMp != original.DstStorageMp {
		t.Fatalf("path fields did not round-trip: got %+v", &restored)
	}
	if restored.GetID() != "task-1" {
		t.Errorf("ID did not round-trip: got %q", restored.GetID())
	}
	if restored.GetState() != tache.StateRunning {
		t.Errorf("state did not round-trip: got %v", restored.GetState())
	}
	retry, maxRetry := restored.GetRetry()
	if retry != 1 || maxRetry != 3 {
		t.Errorf("retry did not round-trip: got %d/%d", retry, maxRetry)
	}
	// driver handles are process-local and must never round-trip through JSON.
	if restored.srcStorage != nil || restored.dstStorage != nil {
		t.Errorf("driver.Driver handles must not round-trip through JSON")
	}
}

// TestMoveTask_JSONRoundTrip mirrors TestCopyTask_JSONRoundTrip for MoveTask,
// which reuses the copy persistence configuration.
func TestMoveTask_JSONRoundTrip(t *testing.T) {
	original := &MoveTask{
		SrcObjPath:   "/a/b.txt",
		DstDirPath:   "/c",
		SrcStorageMp: "/src",
		DstStorageMp: "/dst",
	}
	original.SetID("task-2")

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored MoveTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.SrcObjPath != original.SrcObjPath || restored.DstDirPath != original.DstDirPath ||
		restored.SrcStorageMp != original.SrcStorageMp || restored.DstStorageMp != original.DstStorageMp {
		t.Fatalf("path fields did not round-trip: got %+v", &restored)
	}
	if restored.srcStorage != nil || restored.dstStorage != nil {
		t.Errorf("driver.Driver handles must not round-trip through JSON")
	}
}

// TestCopyTask_MarshalJSONPreservesShape checks CopyTask.MarshalJSON (added
// alongside MoveTask's -- see MoveTask.MarshalJSON for the full rationale)
// produces the exact same key set default reflection would have, for a task
// with retry state set, so the fix can't be silently dropping a field.
func TestCopyTask_MarshalJSONPreservesShape(t *testing.T) {
	original := &CopyTask{
		SrcObjPath:   "/a/b.txt",
		DstDirPath:   "/c",
		SrcStorageMp: "/src",
		DstStorageMp: "/dst",
		ClonePerms:   true,
	}
	original.SetID("copy-shape-1")
	original.SetState(tache.StateErrored)
	original.SetRetry(1, 2)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	type plainCopyTask CopyTask
	plain := (*plainCopyTask)(original)
	wantBytes, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("MarshalJSON changed the persisted shape:\n default-reflection=%v\n custom=%v", want, got)
	}

	var restored CopyTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	retry, maxRetry := restored.GetRetry()
	if retry != 1 || maxRetry != 2 || restored.GetState() != tache.StateErrored {
		t.Fatalf("retry/state did not round-trip: retry=%d/%d state=%v", retry, maxRetry, restored.GetState())
	}
}

// TestCopyTask_PersistRaceUnderRace is a regression guard for the race
// CopyTask.MarshalJSON closes: tache.Manager.persist() (wired via
// WithPersistFunction whenever a manager has real persistence configured --
// the *default* for CopyTaskManager/MoveTaskManager in production, see
// bootstrap.InitTaskManager and conf.Conf.Tasks.Copy.TaskPersistant)
// marshals every task via encoding/json's default reflection, entirely
// unsynchronized, from its own debounced goroutine. Before
// CopyTask.MarshalJSON existed, that reflection read tache.Base's
// State/Retry/MaxRetry concurrently with a worker goroutine's
// SetState/SetRetry calls -- a real data race under -race, invisible to
// tests (like TestCopyTaskManager_ResumesUnfinishedTaskFromPersistedDB
// above) whose tasks finish too fast to reliably overlap a marshal. This
// hammers SetState concurrently with json.Marshal on a single CopyTask to
// force the overlap and prove -race stays clean.
func TestCopyTask_PersistRaceUnderRace(t *testing.T) {
	tsk := &CopyTask{SrcObjPath: "/a", DstDirPath: "/b"}
	tsk.SetID("copy-race-1")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	states := []tache.State{tache.StateRunning, tache.StatePending, tache.StateErrored, tache.StateSucceeded}

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				tsk.SetState(states[i%len(states)])
				tsk.SetRetry(i%3, 3)
				i++
			}
		}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := json.Marshal(tsk); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestMoveTask_PersistRaceUnderRace mirrors TestCopyTask_PersistRaceUnderRace
// for MoveTask.MarshalJSON/MoveTaskManager (which shares the same
// TaskPersistant flag as copy, see conf.Conf.Tasks.Copy.TaskPersistant).
func TestMoveTask_PersistRaceUnderRace(t *testing.T) {
	tsk := &MoveTask{SrcObjPath: "/a", DstDirPath: "/b"}
	tsk.SetID("move-race-1")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	states := []tache.State{tache.StateRunning, tache.StatePending, tache.StateErrored, tache.StateSucceeded}

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				tsk.SetState(states[i%len(states)])
				tsk.SetRetry(i%3, 3)
				i++
			}
		}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := json.Marshal(tsk); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestUploadTask_PersistRaceUnderRace mirrors TestCopyTask_PersistRaceUnderRace
// for UploadTask.MarshalJSON/UploadTaskManager (conf.Conf.Tasks.Upload.TaskPersistant).
func TestUploadTask_PersistRaceUnderRace(t *testing.T) {
	tsk := &UploadTask{DstStorageMp: "/dst", DstDirActualPath: "/dir", FileName: "f.txt"}
	tsk.SetID("upload-race-1")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	states := []tache.State{tache.StateRunning, tache.StatePending, tache.StateErrored, tache.StateSucceeded}

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				tsk.SetState(states[i%len(states)])
				tsk.SetRetry(i%3, 3)
				i++
			}
		}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := json.Marshal(tsk); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestCopyTaskManager_ResumesUnfinishedTaskFromPersistedDB proves out the
// production wiring in bootstrap.InitTaskManager: an unfinished CopyTask
// written to the TaskItem table (as if the process had died mid-copy) is
// loaded and re-run when a *new* manager is constructed with the same
// tache.WithPersistFunction(db.GetTaskDataFunc/UpdateTaskDataFunc) options —
// as long as storages are already loaded (op.CreateStorage below stands in
// for bootstrap.LoadStorages, which cmd/server.go always runs before
// bootstrap.InitTaskManager).
func TestCopyTaskManager_ResumesUnfinishedTaskFromPersistedDB(t *testing.T) {
	const typeKey = "copy-resume-test"
	oldManager := CopyTaskManager
	t.Cleanup(func() { CopyTaskManager = oldManager })

	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "hello.txt"), []byte("resume me"), 0o644); err != nil {
		t.Fatalf("seed src file: %v", err)
	}

	ctx := context.Background()
	if _, err := op.CreateStorage(ctx, model.Storage{
		MountPath: "/resume-src",
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + srcRoot + `"}`,
	}); err != nil {
		t.Fatalf("create src storage: %v", err)
	}
	t.Cleanup(func() {
		if s, err := op.GetStorageByMountPath("/resume-src"); err == nil {
			_ = op.DeleteStorageById(ctx, s.GetStorage().ID)
		}
	})
	if _, err := op.CreateStorage(ctx, model.Storage{
		MountPath: "/resume-dst",
		Driver:    "Local",
		Addition:  `{"root_folder_path":"` + dstRoot + `"}`,
	}); err != nil {
		t.Fatalf("create dst storage: %v", err)
	}
	t.Cleanup(func() {
		if s, err := op.GetStorageByMountPath("/resume-dst"); err == nil {
			_ = op.DeleteStorageById(ctx, s.GetStorage().ID)
		}
	})

	// Simulate a task that was mid-flight (StateRunning) when the process
	// died; tache.Manager.Add demotes StateRunning back to StatePending on
	// load, matching a real restart.
	unfinished := &CopyTask{
		SrcObjPath:   "/hello.txt",
		DstDirPath:   "/",
		SrcStorageMp: "/resume-src",
		DstStorageMp: "/resume-dst",
	}
	unfinished.SetID("resume-task-1")
	unfinished.SetState(tache.StateRunning)

	persisted, err := json.Marshal([]*CopyTask{unfinished})
	if err != nil {
		t.Fatalf("marshal persisted tasks: %v", err)
	}
	if err := db.CreateTaskData(&model.TaskItem{Key: typeKey, PersistData: string(persisted)}); err != nil {
		t.Fatalf("seed persisted task row: %v", err)
	}
	t.Cleanup(func() {
		_ = db.UpdateTaskData(&model.TaskItem{Key: typeKey, PersistData: "[]"})
	})

	// Rebuild the manager the same way bootstrap.InitTaskManager does, just
	// with a short debounce so the test doesn't have to wait out the 3s
	// production default to observe the final persisted write.
	CopyTaskManager = tache.NewManager[*CopyTask](
		tache.WithPersistFunction(db.GetTaskDataFunc(typeKey, true), db.UpdateTaskDataFunc(typeKey, true)),
		tache.WithPersistDebounce(20*time.Millisecond),
	)

	got, ok := CopyTaskManager.GetByID("resume-task-1")
	if !ok {
		t.Fatal("unfinished task was not loaded into the rebuilt manager")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := got.GetState(); s == tache.StateSucceeded || s == tache.StateFailed || s == tache.StateErrored {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.GetState() != tache.StateSucceeded {
		t.Fatalf("resumed task ended in state %v (err=%v), want StateSucceeded", got.GetState(), got.GetErr())
	}

	gotBytes, err := os.ReadFile(filepath.Join(dstRoot, "hello.txt"))
	if err != nil || string(gotBytes) != "resume me" {
		t.Fatalf("resumed copy did not land at destination: content=%q err=%v", gotBytes, err)
	}

	// And the manager must have flushed the post-run state back to the DB
	// (proves the write side of persistence, not just the read/recover side).
	deadline = time.Now().Add(2 * time.Second)
	var row *model.TaskItem
	for time.Now().Before(deadline) {
		row, err = db.GetTaskDataByType(typeKey)
		if err == nil && row.PersistData != string(persisted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("read back persisted row: %v", err)
	}
	if row.PersistData == string(persisted) {
		t.Errorf("persist row was never updated after the task finished")
	}
}
