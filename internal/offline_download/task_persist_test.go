package offline_download

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/xhofe/tache"
)

// TestDownloadTask_MarshalJSONPreservesShape supplements the existing
// TestDownloadTask_JSONRoundTrip (task_test.go) with the two things that
// changed: Retry/MaxRetry now round-trip through DownloadTask.MarshalJSON
// instead of tache's default reflection (see fileNameMu's doc comment on the
// DownloadTask struct, and TaskExtension.PersistSnapshot), and the override
// must not change the key set default reflection would have produced --
// only which goroutine is allowed to read the values it fills in.
func TestDownloadTask_MarshalJSONPreservesShape(t *testing.T) {
	original := &DownloadTask{
		URL:              "https://example.com/f.bin",
		DstStorageMp:     "/dst",
		DstDirActualPath: "/dir",
		FileName:         "f.bin",
		Toolname:         "aria2",
		TempDir:          "/tmp/aria2/uuid",
	}
	original.SetID("download-task-1")
	original.SetState(tache.StateRunning)
	original.SetRetry(1, 3)
	original.GID = "should-not-persist"

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored DownloadTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.URL != original.URL ||
		restored.DstStorageMp != original.DstStorageMp ||
		restored.DstDirActualPath != original.DstDirActualPath ||
		restored.FileName != original.FileName ||
		restored.Toolname != original.Toolname ||
		restored.TempDir != original.TempDir {
		t.Fatalf("persistable fields did not round-trip: got %+v", &restored)
	}
	if restored.GetID() != "download-task-1" || restored.GetState() != tache.StateRunning {
		t.Errorf("tache.Base fields did not round-trip: id=%q state=%v", restored.GetID(), restored.GetState())
	}
	retry, maxRetry := restored.GetRetry()
	if retry != 1 || maxRetry != 3 {
		t.Errorf("retry did not round-trip: got %d/%d", retry, maxRetry)
	}
	if restored.GID != "" {
		t.Errorf("GID (json:\"-\") must not round-trip through JSON, got %q", restored.GID)
	}

	// The custom MarshalJSON must not change the key set default reflection
	// would have produced (only which goroutine is allowed to read the
	// values it fills in) -- compare against a plain type sharing the exact
	// same field layout but no MarshalJSON override.
	type plainDownloadTask DownloadTask
	plain := (*plainDownloadTask)(original)
	// plainDownloadTask has no MarshalJSON of its own (DownloadTask's is not
	// promoted onto a distinct named type), so this goes through default
	// reflection -- exactly what production did before this fix.
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
}

// TestTransferTask_JSONRoundTrip mirrors TestDownloadTask_JSONRoundTrip for
// TransferTask, and likewise checks MarshalJSON preserves the exact shape
// default reflection would have produced.
func TestTransferTask_JSONRoundTrip(t *testing.T) {
	original := &TransferTask{
		SrcFilePath:  "/tmp/aria2/uuid/f.bin",
		DstStorageMp: "/dst",
		DstDirPath:   "/dir",
		DeletePolicy: DeleteOnUploadSucceed,
		FileSize:     123,
	}
	original.SetID("transfer-task-1")
	original.SetState(tache.StateRunning)
	original.SetRetry(2, 4)

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored TransferTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.SrcFilePath != original.SrcFilePath ||
		restored.DstStorageMp != original.DstStorageMp ||
		restored.DstDirPath != original.DstDirPath ||
		restored.DeletePolicy != original.DeletePolicy ||
		restored.FileSize != original.FileSize {
		t.Fatalf("persistable fields did not round-trip: got %+v", &restored)
	}
	if restored.GetID() != "transfer-task-1" || restored.GetState() != tache.StateRunning {
		t.Errorf("tache.Base fields did not round-trip: id=%q state=%v", restored.GetID(), restored.GetState())
	}
	retry, maxRetry := restored.GetRetry()
	if retry != 2 || maxRetry != 4 {
		t.Errorf("retry did not round-trip: got %d/%d", retry, maxRetry)
	}

	type plainTransferTask TransferTask
	plain := (*plainTransferTask)(original)
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
}

// TestDownloadTaskManager_PersistRaceUnderRace is a regression guard for the
// race this whole file's MarshalJSON overrides close: tache.Manager.persist()
// (wired via WithPersistFunction whenever a manager has real persistence
// configured -- the default for OfflineDownloadTaskManager in production, see
// bootstrap.InitOfflineDownloadManager) marshals every task via
// encoding/json's default reflection, entirely unsynchronized, on its own
// debounced goroutine. Before DownloadTask.MarshalJSON existed, that reflection
// read tache.Base's State/Retry/MaxRetry (and DownloadTask's own FileName)
// concurrently with a worker goroutine's SetState/SetRetry/setFileName calls --
// a real data race under -race, invisible to tests that never configure a
// real persist function (as most of this package's tests don't). This test
// hammers SetState (and, via a manual json.Marshal loop mimicking persist's
// own call, the marshal path) concurrently on a single DownloadTask to prove
// -race stays clean.
func TestDownloadTaskManager_PersistRaceUnderRace(t *testing.T) {
	task := &DownloadTask{URL: "https://example.com/f.bin", DstStorageMp: "/dst"}
	task.SetID("race-task-1")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	states := []tache.State{tache.StateRunning, tache.StatePending, tache.StateErrored, tache.StateSucceeded}
	names := []string{"a.bin", "b.bin", "c.bin"}

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				task.SetState(states[i%len(states)])
				task.setFileName(names[i%len(names)])
				i++
			}
		}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := json.Marshal(task); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
