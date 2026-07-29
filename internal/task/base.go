package task

import (
	"context"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/xhofe/tache"
	"sync"
	"time"
)

type TaskExtension struct {
	tache.Base
	ctx          context.Context
	ctxInitMutex sync.Mutex
	Creator      *model.User

	// mu guards startTime/endTime/totalBytes below, and also every method
	// this type defines to shadow tache.Base's own State/Err/Progress
	// accessors (SetState/GetState, SetErr/GetErr, SetProgress/GetProgress
	// further down). tache.Base (github.com/xhofe/tache@v0.1.5) stores
	// State/err/progress as plain unsynchronized fields: its own worker
	// goroutine writes them while running a task (Worker.Execute ->
	// task.Run()), and the HTTP layer (server/handles/task.go's
	// getTaskInfo) reads them from a different goroutine to answer a
	// status poll -- a genuine data race under -race, and in production
	// wherever a client checks task status while the task is still
	// running. tache v0.1.5 ships no mutex and no thread-safe accessor of
	// its own, so rather than forking the dependency this wraps it at the
	// embedding boundary: because TaskExtension defines these method names
	// directly, Go's method-promotion rules make them shadow tache.Base's
	// same-named (deeper-embedded) methods for every concrete task type
	// that embeds TaskExtension (MoveTask, CopyTask, UploadTask,
	// DownloadTask, TransferTask, ...). tache's own Worker.Execute calls
	// task.SetState/SetErr through the generic Task interface, so those
	// calls resolve to the shadowing methods here too, and end up behind
	// the same lock as the read side.
	mu         sync.RWMutex
	startTime  *time.Time
	endTime    *time.Time
	totalBytes int64
	cancelFunc context.CancelFunc
}

func (t *TaskExtension) SetCreator(creator *model.User) {
	t.Creator = creator
	t.Persist()
}

func (t *TaskExtension) GetCreator() *model.User {
	return t.Creator
}

// SetState shadows tache.Base.SetState (see mu's doc comment above) so every
// caller reachable through TaskExtension -- including tache's own worker --
// takes the lock.
//
// Unlike SetErr/SetProgress below, this does NOT call t.Base.SetState (which
// is "b.State = state; b.Persist()" as one call): it writes t.Base.State
// directly -- State is tache.Base's one exported, Persist()-triggering field,
// so this is the one place the write and the Persist() call can be split --
// and releases mu before calling Persist() itself. That matters because
// Persist() -> tache.Base.Persist() -> the manager's debouncePersist can,
// depending on how the owning tache.Manager was configured, run m.persist()
// (-> json.Marshal -> a task's MarshalJSON -> GetState() -> t.mu.RLock())
// SYNCHRONOUSLY on the calling goroutine rather than on its own debounce
// timer's goroutine (see tache's manager.go: this happens whenever
// Options.PersistDebounce is nil, which every manager in this codebase avoids
// -- see task.AssertPersistDebounceAsync, called at every tache.NewManager
// call site with the exact same options, which panics at boot if that ever
// stops being true). Were mu still held at that point, GetState()'s RLock()
// would be a same-goroutine re-entrant lock on a non-reentrant sync.RWMutex:
// a guaranteed deadlock, not a slowdown. Releasing mu before Persist() makes
// SetState safe regardless of debounce configuration; it's defense in depth
// on top of the boot-time assertion, not a substitute for it -- SetErr/
// SetProgress can't get the same treatment (see their comments) and still
// depend on the assertion alone.
func (t *TaskExtension) SetState(state tache.State) {
	t.mu.Lock()
	t.Base.State = state
	t.mu.Unlock()
	t.Base.Persist()
}

// GetState shadows tache.Base.GetState; see SetState.
func (t *TaskExtension) GetState() tache.State {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Base.GetState()
}

// SetErr shadows tache.Base.SetErr; see SetState -- including for the
// Persist()-while-locked hazard SetState's comment describes. Unlike State,
// tache.Base's err field is unexported, so there is no way to split "write
// err" from "call Persist()" the way SetState does: t.Base.SetErr(err) must
// be called as one unit, still under mu. This is safe ONLY because of the
// construction-time guarantee task.AssertPersistDebounceAsync checks (every
// tache.Manager in this codebase resolves to an async debounce, so Persist()
// here never re-enters GetErr()'s RLock() on this same goroutine) -- there is
// no defense-in-depth narrowing available for this one, unlike SetState.
func (t *TaskExtension) SetErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Base.SetErr(err)
}

// GetErr shadows tache.Base.GetErr; see SetState.
func (t *TaskExtension) GetErr() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Base.GetErr()
}

// SetProgress shadows tache.Base.SetProgress; see SetState and SetErr --
// progress is unexported on tache.Base like err, so it has the same
// can't-split-the-Persist()-call constraint and the same reliance on
// task.AssertPersistDebounceAsync (not defense-in-depth narrowing) as SetErr.
func (t *TaskExtension) SetProgress(progress float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Base.SetProgress(progress)
}

// GetProgress shadows tache.Base.GetProgress; see SetState.
func (t *TaskExtension) GetProgress() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Base.GetProgress()
}

// SetRetry shadows tache.Base.SetRetry; see SetState. Retry/MaxRetry are
// exported on tache.Base (json:"retry"/"max_retry"), so unlike err/progress
// (unexported, never touched by json.Marshal) they are also exposed to a
// SEPARATE hazard from the one SetState/GetState close: tache.Manager.persist
// (called whenever a manager is built with WithPersistFunction/PersistPath --
// which is the *default* config for the upload/copy/move/offline_download
// managers, see internal/bootstrap/task.go and offline_download.go) surveys
// every task and does a plain json.Marshal(tasks) on a debounce timer,
// entirely via reflection. Reflection reads the raw struct field, bypassing
// any Go method (including these shadows) -- so persist's read of Retry can
// still race SetRetry's write even after this shadow exists. Closing that
// needs a MarshalJSON override on each concrete task type (see e.g.
// MoveTask.MarshalJSON) that substitutes a locked snapshot -- taken via
// GetState/GetRetry, i.e. these shadows -- for the fields tache.persist()
// would otherwise reflect over unsynchronized. This shadow is what makes that
// snapshot safe to take; it does not by itself close the persist race.
func (t *TaskExtension) SetRetry(retry int, maxRetry int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Base.SetRetry(retry, maxRetry)
}

// GetRetry shadows tache.Base.GetRetry; see SetRetry.
func (t *TaskExtension) GetRetry() (int, int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Base.GetRetry()
}

// PersistSnapshot returns a point-in-time, lock-synchronized snapshot of the
// tache.Base fields that are BOTH exported (so tache's reflection-based
// persist() would otherwise read them unsynchronized) AND mutated after
// construction (so a worker goroutine can be writing them concurrently):
// State, Retry, MaxRetry. GetID is deliberately not included -- SetID only
// ever runs once, inside tache.Manager.Add, strictly before the task becomes
// visible to persist()'s m.GetAll() (Add stores it in the task map only after
// SetID returns), so there is no concurrent-mutation window for it to close.
//
// Concrete task types call this from their own MarshalJSON override (see
// MoveTask.MarshalJSON for the pattern) to substitute these snapshotted
// values for tache's default per-field reflection, which reads State/Retry/
// MaxRetry directly off the struct with no synchronization at all.
func (t *TaskExtension) PersistSnapshot() (state tache.State, retry int, maxRetry int) {
	state = t.GetState()
	retry, maxRetry = t.GetRetry()
	return
}

func (t *TaskExtension) SetStartTime(startTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.startTime = &startTime
}

func (t *TaskExtension) GetStartTime() *time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.startTime
}

func (t *TaskExtension) SetEndTime(endTime time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endTime = &endTime
}

func (t *TaskExtension) GetEndTime() *time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.endTime
}

func (t *TaskExtension) ClearEndTime() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endTime = nil
}

func (t *TaskExtension) SetTotalBytes(totalBytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.totalBytes = totalBytes
}

func (t *TaskExtension) GetTotalBytes() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.totalBytes
}

// SetCancelFunc shadows tache.Base.SetCancelFunc so Cancel (below) can invoke
// the cancel func without reaching into tache.Base's unexported cancel field
// (which it has no accessor for). It also still forwards to Base.SetCancelFunc
// to keep Base's own copy consistent, in case anything reaches it through a
// bare *tache.Base.
func (t *TaskExtension) SetCancelFunc(cancelFunc context.CancelFunc) {
	t.mu.Lock()
	t.cancelFunc = cancelFunc
	t.mu.Unlock()
	t.Base.SetCancelFunc(cancelFunc)
}

// Cancel shadows tache.Base.Cancel. tache.Base.Cancel calls its own SetState
// directly on the *Base receiver (Go method calls don't dispatch virtually),
// so leaving it unshadowed would let a cancel race unguarded against
// getTaskInfo's GetState even after the SetState override above. Reimplemented
// here against our own guarded SetState and the cancelFunc mirrored by
// SetCancelFunc, instead of delegating to Base.Cancel.
func (t *TaskExtension) Cancel() {
	t.SetState(tache.StateCanceling)
	t.mu.RLock()
	cancel := t.cancelFunc
	t.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (t *TaskExtension) Ctx() context.Context {
	if t.ctx == nil {
		t.ctxInitMutex.Lock()
		if t.ctx == nil {
			t.ctx = context.WithValue(t.Base.Ctx(), ctxkey.User, t.Creator)
		}
		t.ctxInitMutex.Unlock()
	}
	return t.ctx
}

func (t *TaskExtension) ReinitCtx() {
	if !conf.Conf.Tasks.AllowRetryCanceled {
		return
	}
	select {
	case <-t.Base.Ctx().Done():
		ctx, cancel := context.WithCancel(context.Background())
		t.SetCtx(ctx)
		t.SetCancelFunc(cancel)
		t.ctx = nil
	default:
	}
}

type TaskExtensionInfo interface {
	tache.TaskWithInfo
	GetCreator() *model.User
	GetStartTime() *time.Time
	GetEndTime() *time.Time
	GetTotalBytes() int64
}

// TaskInfoWithI18n is implemented by tasks whose display name and status are
// expressed as i18n catalog keys plus "{name}"-placeholder params. tache
// persists a task by json-marshalling its struct fields (not GetName/GetStatus),
// so these accessors let the API layer re-render name/status in the viewer's
// negotiated locale at read time instead of freezing a pre-rendered English
// string. GetName()/GetStatus() still return the English rendering for logging
// and back-compat. A key returned as "" means "no keyed form, use GetName/
// GetStatus verbatim". Args are name/value pairs matching i18n.T's signature.
type TaskInfoWithI18n interface {
	GetNameKey() string
	GetNameArgs() []any
	GetStatusKey() string
	GetStatusArgs() []any
}

// AssertPersistDebounceAsync panics if opts would make tache.NewManager
// resolve to a nil Options.PersistDebounce. Call it with the EXACT same opts
// slice passed to tache.NewManager, before that call (every call site in
// internal/bootstrap does this).
//
// Why this matters: tache.Manager's constructor (manager.go) picks between
// two debouncePersist implementations depending on PersistDebounce:
//
//	if opts.PersistDebounce != nil {
//	    debouncePersist = newDebounce(func() { m.persist() }, *opts.PersistDebounce)  // async: time.AfterFunc, own goroutine
//	} else {
//	    debouncePersist = func() { m.persist() }  // SYNCHRONOUS, caller's own goroutine
//	}
//
// tache.DefaultOptions() always sets PersistDebounce to a non-nil 3s default,
// and every per-field Option this codebase uses (WithWorks, WithMaxRetry,
// WithPersistFunction, WithRunning, ...) leaves it untouched -- so today
// PersistDebounce is never nil and debouncePersist is always the async
// variant. The one Option that COULD null it back out is tache.WithOptions
// (it replaces the whole Options struct wholesale: any field the caller's
// literal doesn't set, including PersistDebounce, reverts to its zero value,
// nil for a pointer) -- this codebase never calls it, but nothing stops a
// future change from doing so.
//
// If PersistDebounce ever did resolve to nil, TaskExtension.SetState/SetErr/
// SetProgress would deadlock: they call tache.Base.Persist() while holding
// (SetErr/SetProgress) or having just released (SetState) TaskExtension.mu,
// and Persist() -> debouncePersist() would now run m.persist() -> a task's
// MarshalJSON -> GetState()/GetErr()/GetProgress() -> t.mu.RLock() -- for
// SetErr/SetProgress specifically, that RLock() would be on the SAME
// goroutine that still holds mu's write lock via the surrounding Lock()/
// defer Unlock(), and sync.RWMutex is not reentrant: guaranteed deadlock, not
// a slowdown. (SetState narrows its own critical section so it wouldn't
// deadlock even then -- see its comment -- but SetErr/SetProgress can't,
// since tache.Base's err/progress fields are unexported.)
//
// This re-implements tache's own resolution (DefaultOptions + applying opts)
// using only tache's exported Options/Option/DefaultOptions surface, so it
// requires no fork and stays byte-for-byte in sync with what NewManager will
// actually do with the same opts.
func AssertPersistDebounceAsync(opts ...tache.Option) {
	o := tache.DefaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	if o.PersistDebounce == nil {
		panic("task: tache manager options resolve to a nil PersistDebounce, which makes " +
			"tache.Base.Persist() run synchronously on the calling goroutine instead of its " +
			"own debounce-timer goroutine -- this deadlocks TaskExtension.SetErr/SetProgress " +
			"(see AssertPersistDebounceAsync's doc comment in internal/task/base.go); pass " +
			"tache.WithPersistDebounce(d) explicitly, or avoid a tache.WithOptions(...) call " +
			"that drops the default")
	}
}
