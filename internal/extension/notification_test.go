package extension

import (
	"context"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// recordingChannel records delivered notices; panicChannel always panics.
type recordingChannel struct {
	mu  sync.Mutex
	got []ext.Notice
}

func (c *recordingChannel) Manifest() ext.Manifest               { return ext.Manifest{} }
func (c *recordingChannel) Init(context.Context, ext.Host) error { return nil }
func (c *recordingChannel) Shutdown(context.Context) error       { return nil }
func (c *recordingChannel) Notify(_ context.Context, n ext.Notice) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, n)
	return nil
}

type panicChannel struct{}

func (panicChannel) Manifest() ext.Manifest               { return ext.Manifest{} }
func (panicChannel) Init(context.Context, ext.Host) error { return nil }
func (panicChannel) Shutdown(context.Context) error       { return nil }
func (panicChannel) Notify(context.Context, ext.Notice) error {
	panic("boom")
}

// errChannel always returns an error from Notify (not a panic) — proves error
// isolation as distinct from panic isolation.
type errChannel struct{}

func (errChannel) Manifest() ext.Manifest               { return ext.Manifest{} }
func (errChannel) Init(context.Context, ext.Host) error { return nil }
func (errChannel) Shutdown(context.Context) error       { return nil }
func (errChannel) Notify(context.Context, ext.Notice) error {
	return ext.NewError(ext.ErrCodeObjectNotFound, "channel down")
}

// notificationEntry builds an enabled Kind=NotificationChannel entry.
func notificationEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindNotification}},
		Kind:     ext.KindNotification,
		enabled:  true,
	}
}

func TestDispatchNotificationDeliversToEnabledChannels(t *testing.T) {
	r := NewRegistry()
	a := &recordingChannel{}
	b := &recordingChannel{}

	if err := r.Register(notificationEntry("chan.a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(notificationEntry("chan.b")); err != nil {
		t.Fatal(err)
	}
	RegisterNotificationInstance("chan.a", a)
	RegisterNotificationInstance("chan.b", b)
	t.Cleanup(func() {
		RegisterNotificationInstance("chan.a", nil)
		RegisterNotificationInstance("chan.b", nil)
	})

	n := ext.Notice{Level: ext.NoticeInfo, Title: "hello", Source: "task"}
	DispatchNotification(context.Background(), r, n)

	if len(a.got) != 1 || a.got[0].Title != "hello" {
		t.Fatalf("chan.a should have received the notice, got %+v", a.got)
	}
	if len(b.got) != 1 || b.got[0].Title != "hello" {
		t.Fatalf("chan.b should have received the notice, got %+v", b.got)
	}
}

func TestDispatchNotificationIsolatesPanicAndContinues(t *testing.T) {
	r := NewRegistry()
	good := &recordingChannel{}

	if err := r.Register(notificationEntry("chan.panic")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(notificationEntry("chan.good")); err != nil {
		t.Fatal(err)
	}
	RegisterNotificationInstance("chan.panic", panicChannel{})
	RegisterNotificationInstance("chan.good", good)
	t.Cleanup(func() {
		RegisterNotificationInstance("chan.panic", nil)
		RegisterNotificationInstance("chan.good", nil)
	})

	// Must not panic out, and the good channel still gets the notice.
	DispatchNotification(context.Background(), r, ext.Notice{Level: ext.NoticeError, Title: "boom source"})

	if len(good.got) != 1 {
		t.Fatalf("a panicking channel must not stop delivery to others, good.got=%+v", good.got)
	}
}

func TestDispatchNotificationIsolatesErrorAndContinues(t *testing.T) {
	r := NewRegistry()
	good := &recordingChannel{}

	if err := r.Register(notificationEntry("chan.err")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(notificationEntry("chan.good2")); err != nil {
		t.Fatal(err)
	}
	RegisterNotificationInstance("chan.err", errChannel{})
	RegisterNotificationInstance("chan.good2", good)
	t.Cleanup(func() {
		RegisterNotificationInstance("chan.err", nil)
		RegisterNotificationInstance("chan.good2", nil)
	})

	DispatchNotification(context.Background(), r, ext.Notice{Level: ext.NoticeWarn, Title: "x"})

	if len(good.got) != 1 {
		t.Fatalf("an erroring channel must not stop delivery to others, good.got=%+v", good.got)
	}
}

func TestDispatchNotificationSkipsDisabled(t *testing.T) {
	r := NewRegistry()
	c := &recordingChannel{}
	e := notificationEntry("chan.disabled")
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterNotificationInstance("chan.disabled", c)
	t.Cleanup(func() { RegisterNotificationInstance("chan.disabled", nil) })

	DispatchNotification(context.Background(), r, ext.Notice{Level: ext.NoticeInfo, Title: "x"})
	if len(c.got) != 0 {
		t.Fatalf("disabled channel must not receive notices, got %+v", c.got)
	}
}

// grpcNotificationEntry builds a declared, enabled, ExecGRPC
// Kind=NotificationChannel entry — the shape resolveNotification requires to
// even attempt the factory branch.
func grpcNotificationEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindNotification}},
		Kind:     ext.KindNotification,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-notification",
	}
}

// TestResolveNotificationUsesFactory pins the gRPC-dispatch seam: an ExecGRPC
// entry with a binPath and no in-process instance must resolve through the
// installed notificationFactory (the plugins.go init wires this to
// pluginproc.Default.AcquireNotification). Without a factory the same entry
// resolves to nil (no host-side handler). Mirrors
// TestResolveEventHookUsesFactory one Kind over.
func TestResolveNotificationUsesFactory(t *testing.T) {
	saved := notificationFactory
	t.Cleanup(func() { notificationFactory = saved })

	e := grpcNotificationEntry("chan.grpc")

	// No factory wired → no host-side handler → nil.
	notificationFactory = nil
	if ch, _ := resolveNotification(e); ch != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", ch)
	}

	// Factory wired → the ExecGRPC branch is hit and its channel is returned.
	stub := &recordingChannel{}
	var released bool
	notificationFactory = func(got *Entry) (ext.Notification, func(), error) {
		if got.ID() != "chan.grpc" || got.BinPath() != "/bin/example-notification" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	ch, release := resolveNotification(e)
	if ch != stub {
		t.Fatalf("resolveNotification must return the factory's channel, got %T", ch)
	}
	release()
	if !released {
		t.Fatal("resolveNotification must return the factory's release func")
	}
}

// TestDispatchNotificationFansOutToGRPCResolvedChannel proves
// DispatchNotification's factory branch is wired end to end: a granted,
// ExecGRPC entry with no in-process instance still receives the notice,
// through notificationFactory.
func TestDispatchNotificationFansOutToGRPCResolvedChannel(t *testing.T) {
	saved := notificationFactory
	t.Cleanup(func() { notificationFactory = saved })

	r := NewRegistry()
	e := grpcNotificationEntry("chan.grpc.dispatch")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := &recordingChannel{}
	var released bool
	notificationFactory = func(got *Entry) (ext.Notification, func(), error) {
		return stub, func() { released = true }, nil
	}

	DispatchNotification(context.Background(), r, ext.Notice{Level: ext.NoticeInfo, Title: "d"})

	if len(stub.got) != 1 || stub.got[0].Title != "d" {
		t.Fatalf("gRPC-resolved channel should have received the notice, got %+v", stub.got)
	}
	if !released {
		t.Fatal("DispatchNotification must call the factory's release func")
	}
}
