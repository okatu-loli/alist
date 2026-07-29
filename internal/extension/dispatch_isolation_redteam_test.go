package extension

// F1 red-team: EventHook / ScheduledJob isolation + non-veto suite.
//
// Two guarantees under attack (design §2.5, ext.EventHook / ext.ScheduledJob):
//   - EventHook is a POST-COMMIT NOTIFICATION, never a veto: a hook returning an
//     error or panicking cannot fail/undo the user operation nor stop delivery
//     to the other hooks. DispatchEvent has no return value — it structurally
//     cannot report a veto back to the fs mutation path.
//   - A ScheduledJob's panic is isolated by the host and does not propagate.
//
// All are permanent regressions.

import (
	"context"
	"sync/atomic"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// evHook is an in-process EventHook that records delivery and can be told to
// error or panic, to prove neither escapes the host's isolation.
type evHook struct {
	calls   int32
	fail    bool
	panicOn bool
}

func (h *evHook) Manifest() ext.Manifest               { return ext.Manifest{} }
func (h *evHook) Init(context.Context, ext.Host) error { return nil }
func (h *evHook) Shutdown(context.Context) error       { return nil }
func (h *evHook) OnEvent(_ context.Context, _ ext.Event) error {
	atomic.AddInt32(&h.calls, 1)
	if h.panicOn {
		panic("malicious hook panic")
	}
	if h.fail {
		return ext.ErrPermissionDenied
	}
	return nil
}

func eventHookEntry(id string, apis ...string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindEventHook},
			Permissions: ext.Permissions{HostAPIs: apis}},
		Kind:    ext.KindEventHook,
		enabled: true,
	}
}

// TestReg_EventHookErrorAndPanicIsolatedNonVeto: two hostile hooks (one erroring,
// one panicking) plus a well-behaved one, all granted "events". DispatchEvent
// must return normally (no panic escapes), and the well-behaved hook must still
// be delivered to — one hook cannot suppress the others or veto the (already
// committed) operation. DispatchEvent has no error return, so it is structurally
// incapable of undoing the fs op.
func TestReg_EventHookErrorAndPanicIsolatedNonVeto(t *testing.T) {
	r := NewRegistry()
	bad := &evHook{fail: true}
	boom := &evHook{panicOn: true}
	good := &evHook{}
	for id, h := range map[string]*evHook{"ev.bad": bad, "ev.boom": boom, "ev.good": good} {
		if err := r.Register(eventHookEntry(id, CapEvents)); err != nil {
			t.Fatal(err)
		}
		RegisterEventHookInstance(id, h)
	}
	t.Cleanup(func() {
		RegisterEventHookInstance("ev.bad", nil)
		RegisterEventHookInstance("ev.boom", nil)
		RegisterEventHookInstance("ev.good", nil)
	})

	// Must not panic and must return (void) — the fs path continues regardless.
	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSRemove, Path: "/x"})

	if atomic.LoadInt32(&good.calls) != 1 {
		t.Fatalf("well-behaved hook not delivered despite a panicking/erroring peer (calls=%d)", good.calls)
	}
	if atomic.LoadInt32(&boom.calls) != 1 || atomic.LoadInt32(&bad.calls) != 1 {
		t.Fatalf("hostile hooks not each invoked once (boom=%d bad=%d)", boom.calls, bad.calls)
	}
}

// TestReg_EventHookUngrantedNotDelivered: a hook that was NOT granted "events"
// receives nothing — the shape of a user's fs activity (paths, actor) is not
// disclosed to an unapproved plugin. This is the declaration-gated dispatch
// (interim source = manifest; see grant_gate.go). A plugin declaring nothing is
// denied delivery.
func TestReg_EventHookUngrantedNotDelivered(t *testing.T) {
	r := NewRegistry()
	granted := &evHook{}
	ungranted := &evHook{}
	if err := r.Register(eventHookEntry("ev.granted", CapEvents)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(eventHookEntry("ev.ungranted")); err != nil { // no CapEvents
		t.Fatal(err)
	}
	RegisterEventHookInstance("ev.granted", granted)
	RegisterEventHookInstance("ev.ungranted", ungranted)
	t.Cleanup(func() {
		RegisterEventHookInstance("ev.granted", nil)
		RegisterEventHookInstance("ev.ungranted", nil)
	})

	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSMove, Path: "/a", DstPath: "/b", Actor: "alice"})

	if atomic.LoadInt32(&ungranted.calls) != 0 {
		t.Fatalf("ungranted hook received an event (leak): calls=%d", ungranted.calls)
	}
	if atomic.LoadInt32(&granted.calls) != 1 {
		t.Fatalf("granted hook not delivered: calls=%d", granted.calls)
	}
}

// jobStub is an in-process ScheduledJob that panics on Run.
type jobStub struct{ ran int32 }

func (j *jobStub) Manifest() ext.Manifest               { return ext.Manifest{} }
func (j *jobStub) Init(context.Context, ext.Host) error { return nil }
func (j *jobStub) Shutdown(context.Context) error       { return nil }
func (j *jobStub) Schedule() string                     { return "@every 1h" }
func (j *jobStub) Run(context.Context) error {
	atomic.AddInt32(&j.ran, 1)
	panic("malicious job panic")
}

// TestReg_ScheduledJobPanicIsolated: a job whose Run panics must not crash the
// scheduler goroutine's supervisor — runJobOnce recovers it. If the panic
// escaped, this test process would crash.
func TestReg_ScheduledJobPanicIsolated(t *testing.T) {
	j := &jobStub{}
	// runJobOnce is the per-tick invocation with panic+error isolation.
	runJobOnce(context.Background(), "job.boom", j)
	if atomic.LoadInt32(&j.ran) != 1 {
		t.Fatalf("job Run not invoked: ran=%d", j.ran)
	}
	// A second invocation still works (supervisor loop is not poisoned).
	runJobOnce(context.Background(), "job.boom", j)
	if atomic.LoadInt32(&j.ran) != 2 {
		t.Fatalf("job not re-invoked after a panic: ran=%d", j.ran)
	}
}

// TestReg_ScheduleParsingRejectsAbuse: a job cannot smuggle a busy-loop cadence.
// A non-positive or unparseable schedule is refused (the scheduler disables the
// job) rather than defaulting to a hot interval.
func TestReg_ScheduleParsingRejectsAbuse(t *testing.T) {
	for _, bad := range []string{"@every 0s", "@every -1s", "@every 0", "", "* * * * *", "@yearly", "@every"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("ParseSchedule(%q): want error, got nil", bad)
		}
	}
	for _, ok := range []string{"@every 30s", "@hourly", "@daily", "@weekly"} {
		if _, err := ParseSchedule(ok); err != nil {
			t.Errorf("ParseSchedule(%q): want ok, got %v", ok, err)
		}
	}
}
