package extension

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

func TestParseSchedule(t *testing.T) {
	cases := []struct {
		spec string
		want time.Duration
		ok   bool
	}{
		{"@every 30s", 30 * time.Second, true},
		{"@every 1h", time.Hour, true},
		{"@every 250ms", 250 * time.Millisecond, true},
		{"@hourly", time.Hour, true},
		{"@daily", 24 * time.Hour, true},
		{"@weekly", 7 * 24 * time.Hour, true},
		{"", 0, false},
		{"garbage", 0, false},
		{"@every 0s", 0, false},  // non-positive disables
		{"@every -5s", 0, false}, // negative disables
		{"* * * * *", 0, false},  // 5-field cron is a documented seam, not yet supported
	}
	for _, c := range cases {
		got, err := ParseSchedule(c.spec)
		if c.ok {
			if err != nil || got != c.want {
				t.Errorf("ParseSchedule(%q) = %v, %v; want %v, nil", c.spec, got, err, c.want)
			}
		} else if err == nil {
			t.Errorf("ParseSchedule(%q) = %v, nil; want error", c.spec, got)
		}
	}
}

// tickJob counts Run invocations.
type tickJob struct {
	spec  string
	count int64
}

func (j *tickJob) Manifest() ext.Manifest               { return ext.Manifest{} }
func (j *tickJob) Init(context.Context, ext.Host) error { return nil }
func (j *tickJob) Shutdown(context.Context) error       { return nil }
func (j *tickJob) Schedule() string                     { return j.spec }
func (j *tickJob) Run(context.Context) error            { atomic.AddInt64(&j.count, 1); return nil }

// panicJob panics every Run.
type panicJob struct{ spec string }

func (panicJob) Manifest() ext.Manifest               { return ext.Manifest{} }
func (panicJob) Init(context.Context, ext.Host) error { return nil }
func (panicJob) Shutdown(context.Context) error       { return nil }
func (j panicJob) Schedule() string                   { return j.spec }
func (panicJob) Run(context.Context) error            { panic("job boom") }

func jobEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindScheduledJob}},
		Kind:     ext.KindScheduledJob,
		enabled:  true,
	}
}

func TestSchedulerTicksAndIsolatesPanics(t *testing.T) {
	r := NewRegistry()
	good := &tickJob{spec: "@every 40ms"}

	if err := r.Register(jobEntry("job.good")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(jobEntry("job.panic")); err != nil {
		t.Fatal(err)
	}
	RegisterScheduledJobInstance("job.good", good)
	RegisterScheduledJobInstance("job.panic", panicJob{spec: "@every 40ms"})
	t.Cleanup(func() {
		RegisterScheduledJobInstance("job.good", nil)
		RegisterScheduledJobInstance("job.panic", nil)
	})

	sched := NewScheduler(r)
	ctx, cancel := context.WithCancel(context.Background())
	sched.Start(ctx)

	time.Sleep(300 * time.Millisecond)
	cancel()
	sched.Wait()

	// ~40ms cadence over 300ms should fire several times; a panicking peer job
	// must not have stopped the good job's ticker.
	if n := atomic.LoadInt64(&good.count); n < 3 {
		t.Fatalf("good job fired %d times, want >= 3 (panicking peer must not stop it)", n)
	}
}

// slowJob blocks so the scheduler must skip overlapping ticks.
type slowJob struct {
	spec    string
	running int32
	overlap int32
	release chan struct{}
}

func (j *slowJob) Manifest() ext.Manifest               { return ext.Manifest{} }
func (j *slowJob) Init(context.Context, ext.Host) error { return nil }
func (j *slowJob) Shutdown(context.Context) error       { return nil }
func (j *slowJob) Schedule() string                     { return j.spec }
func (j *slowJob) Run(context.Context) error {
	if atomic.AddInt32(&j.running, 1) > 1 {
		atomic.AddInt32(&j.overlap, 1)
	}
	<-j.release
	atomic.AddInt32(&j.running, -1)
	return nil
}

func TestSchedulerSkipsOverlappingRuns(t *testing.T) {
	r := NewRegistry()
	job := &slowJob{spec: "@every 20ms", release: make(chan struct{})}
	if err := r.Register(jobEntry("job.slow")); err != nil {
		t.Fatal(err)
	}
	RegisterScheduledJobInstance("job.slow", job)
	t.Cleanup(func() { RegisterScheduledJobInstance("job.slow", nil) })

	sched := NewScheduler(r)
	ctx, cancel := context.WithCancel(context.Background())
	sched.Start(ctx)

	// Let several 20ms ticks fire while the first Run is blocked.
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&job.overlap); got != 0 {
		t.Fatalf("scheduler ran overlapping invocations (%d) — should skip while a Run is in flight", got)
	}
	// Unblock and shut down.
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(job.release) }) }
	cancel()
	closeRelease()
	sched.Wait()
}

// grpcJobEntry builds a declared, enabled, ExecGRPC Kind=ScheduledJob entry —
// the shape resolveScheduledJob requires to even attempt the factory branch.
func grpcJobEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindScheduledJob}},
		Kind:     ext.KindScheduledJob,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-scheduledjob",
	}
}

// TestResolveScheduledJobUsesFactory pins the gRPC-dispatch seam: an ExecGRPC
// entry with a binPath and no in-process instance must resolve through the
// installed scheduledJobFactory (the plugins.go init wires this to
// pluginproc.Default.AcquireScheduledJob). Without a factory the same entry
// resolves to nil (no host-side handler). Mirrors TestResolveEventHookUsesFactory
// one Kind over.
func TestResolveScheduledJobUsesFactory(t *testing.T) {
	saved := scheduledJobFactory
	t.Cleanup(func() { scheduledJobFactory = saved })

	e := grpcJobEntry("job.grpc")

	// No factory wired → no host-side handler → nil.
	scheduledJobFactory = nil
	if job, _ := resolveScheduledJob(e); job != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", job)
	}

	// Factory wired → the ExecGRPC branch is hit and its job is returned.
	stub := &tickJob{spec: "@every 1h"}
	var released bool
	scheduledJobFactory = func(got *Entry) (ext.ScheduledJob, func(), error) {
		if got.ID() != "job.grpc" || got.BinPath() != "/bin/example-scheduledjob" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	job, release := resolveScheduledJob(e)
	if job != stub {
		t.Fatalf("resolveScheduledJob must return the factory's job, got %T", job)
	}
	release()
	if !released {
		t.Fatal("resolveScheduledJob must return the factory's release func")
	}
}

// TestSchedulerRunsGRPCResolvedJob proves Scheduler.Start's factory branch is
// wired end to end: an ExecGRPC entry with no in-process instance still gets
// ticked, through scheduledJobFactory.
func TestSchedulerRunsGRPCResolvedJob(t *testing.T) {
	saved := scheduledJobFactory
	t.Cleanup(func() { scheduledJobFactory = saved })

	r := NewRegistry()
	e := grpcJobEntry("job.grpc.run")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := &tickJob{spec: "@every 30ms"}
	var released bool
	scheduledJobFactory = func(got *Entry) (ext.ScheduledJob, func(), error) {
		return stub, func() { released = true }, nil
	}

	sched := NewScheduler(r)
	ctx, cancel := context.WithCancel(context.Background())
	sched.Start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()
	sched.Wait()

	if n := atomic.LoadInt64(&stub.count); n < 2 {
		t.Fatalf("gRPC-resolved job fired %d times, want >= 2", n)
	}
	if !released {
		t.Fatal("Scheduler must call the factory's release func on shutdown")
	}
}
