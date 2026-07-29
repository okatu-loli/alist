package bootstrap

import (
	"testing"
	"time"

	"github.com/xhofe/tache"
)

// slowTestTask blocks in Run() until release is closed, signaling on start
// the instant it begins executing -- just enough of tache.Task to drive
// startPausedManager without pulling in fs.CopyTask's storage/DB deps.
type slowTestTask struct {
	tache.Base
	start   chan struct{}
	release chan struct{}
}

func (t *slowTestTask) Run() error {
	t.start <- struct{}{}
	<-t.release
	return nil
}

// TestStartPausedManager_DoesNotRunBeforeStarted guards the reason
// InitTaskManager builds managers with tache.WithRunning(false) in the first
// place: a manager recovered from persisted DB state must not dispatch any
// task until startPausedManager (called only once storages have finished
// loading) says so, or a resumed Copy/Move/Upload task can race
// bootstrap.LoadStorages's background goroutine and fail with "no mount
// path for a storage" before the destination is even registered.
func TestStartPausedManager_DoesNotRunBeforeStarted(t *testing.T) {
	starts := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)

	m := tache.NewManager[*slowTestTask](tache.WithWorks(2), tache.WithRunning(false))
	m.Add(&slowTestTask{start: starts, release: release})

	select {
	case <-starts:
		t.Fatal("task started even though the manager was built with WithRunning(false)")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestStartPausedManager_AchievesFullConcurrency guards the other half:
// tache's Manager.Start only claims one worker (it calls its internal next()
// exactly once), so naively Start()-ing a manager that recovered many queued
// tasks would silently serialize it to concurrency 1 forever -- nothing else
// ever calls next() enough times to claim the rest of the pool. This is
// exactly what an earlier version of this fix did wrong, caught by a live
// restart test that saw resumed copy throughput drop from ~500 files/sec to
// ~50-190 files/sec after resuming a paused manager. startPausedManager's
// zero-then-bump sequence must bring all N workers up at once.
func TestStartPausedManager_AchievesFullConcurrency(t *testing.T) {
	const n = 4
	starts := make(chan struct{}, n)
	release := make(chan struct{})
	defer close(release)

	m := tache.NewManager[*slowTestTask](tache.WithWorks(n), tache.WithRunning(false))
	for i := 0; i < n; i++ {
		m.Add(&slowTestTask{start: starts, release: release})
	}

	startPausedManager(m, int64(n))

	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-starts:
		case <-deadline:
			t.Fatalf("only %d/%d tasks became concurrently active within the deadline", i, n)
		}
	}
}
