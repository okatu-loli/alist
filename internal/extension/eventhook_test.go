package extension

import (
	"context"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// recordingHook records delivered events; panicHook always panics.
type recordingHook struct {
	mu  sync.Mutex
	got []ext.Event
}

func (h *recordingHook) Manifest() ext.Manifest               { return ext.Manifest{} }
func (h *recordingHook) Init(context.Context, ext.Host) error { return nil }
func (h *recordingHook) Shutdown(context.Context) error       { return nil }
func (h *recordingHook) OnEvent(_ context.Context, e ext.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, e)
	return nil
}

type panicHook struct{}

func (panicHook) Manifest() ext.Manifest               { return ext.Manifest{} }
func (panicHook) Init(context.Context, ext.Host) error { return nil }
func (panicHook) Shutdown(context.Context) error       { return nil }
func (panicHook) OnEvent(context.Context, ext.Event) error {
	panic("boom")
}

// hookEntry builds an enabled Kind=EventHook entry declaring the given host_apis.
func hookEntry(id string, apis ...string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindEventHook},
			Permissions: ext.Permissions{HostAPIs: apis}},
		Kind:    ext.KindEventHook,
		enabled: true,
	}
}

func TestDispatchEventDeliversToGrantedHooksOnly(t *testing.T) {
	r := NewRegistry()
	granted := &recordingHook{}
	ungranted := &recordingHook{}

	if err := r.Register(hookEntry("hook.granted", CapEvents)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(hookEntry("hook.ungranted")); err != nil { // no "events" grant
		t.Fatal(err)
	}
	RegisterEventHookInstance("hook.granted", granted)
	RegisterEventHookInstance("hook.ungranted", ungranted)
	t.Cleanup(func() {
		RegisterEventHookInstance("hook.granted", nil)
		RegisterEventHookInstance("hook.ungranted", nil)
	})

	ev := ext.Event{Type: ext.EventFSRemove, Path: "/x.txt", Actor: "bob"}
	DispatchEvent(context.Background(), r, ev)

	if len(granted.got) != 1 || granted.got[0].Path != "/x.txt" {
		t.Fatalf("granted hook should have received the event, got %+v", granted.got)
	}
	if len(ungranted.got) != 0 {
		t.Fatalf("ungranted hook must NOT receive events (no 'events' grant), got %+v", ungranted.got)
	}
}

func TestDispatchEventIsolatesPanicAndContinues(t *testing.T) {
	r := NewRegistry()
	good := &recordingHook{}

	if err := r.Register(hookEntry("hook.panic", CapEvents)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(hookEntry("hook.good", CapEvents)); err != nil {
		t.Fatal(err)
	}
	RegisterEventHookInstance("hook.panic", panicHook{})
	RegisterEventHookInstance("hook.good", good)
	t.Cleanup(func() {
		RegisterEventHookInstance("hook.panic", nil)
		RegisterEventHookInstance("hook.good", nil)
	})

	// Must not panic out, and the good hook still gets the event.
	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSMkdir, Path: "/d"})

	if len(good.got) != 1 {
		t.Fatalf("a panicking hook must not stop delivery to others, good.got=%+v", good.got)
	}
}

func TestDispatchEventSkipsDisabled(t *testing.T) {
	r := NewRegistry()
	h := &recordingHook{}
	e := hookEntry("hook.disabled", CapEvents)
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	RegisterEventHookInstance("hook.disabled", h)
	t.Cleanup(func() { RegisterEventHookInstance("hook.disabled", nil) })

	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSPut, Path: "/d"})
	if len(h.got) != 0 {
		t.Fatalf("disabled hook must not receive events, got %+v", h.got)
	}
}

// grpcHookEntry builds a declared, enabled, ExecGRPC Kind=EventHook entry —
// the shape resolveEventHook requires to even attempt the factory branch.
func grpcHookEntry(id string, apis ...string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindEventHook},
			Permissions: ext.Permissions{HostAPIs: apis}},
		Kind:     ext.KindEventHook,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-eventhook",
	}
}

// TestResolveEventHookUsesFactory pins the gRPC-dispatch seam: an ExecGRPC
// entry with a binPath and no in-process instance must resolve through the
// installed eventHookFactory (the plugins.go init wires this to
// pluginproc.Default.AcquireEventHook). Without a factory the same entry
// resolves to nil (no host-side handler). Mirrors TestResolveAPIRouteUsesFactory
// one Kind over.
func TestResolveEventHookUsesFactory(t *testing.T) {
	saved := eventHookFactory
	t.Cleanup(func() { eventHookFactory = saved })

	e := grpcHookEntry("hook.grpc", CapEvents)

	// No factory wired → no host-side handler → nil.
	eventHookFactory = nil
	if hook, _ := resolveEventHook(e); hook != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", hook)
	}

	// Factory wired → the ExecGRPC branch is hit and its hook is returned.
	stub := &recordingHook{}
	var released bool
	eventHookFactory = func(got *Entry) (ext.EventHook, func(), error) {
		if got.ID() != "hook.grpc" || got.BinPath() != "/bin/example-eventhook" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	hook, release := resolveEventHook(e)
	if hook != stub {
		t.Fatalf("resolveEventHook must return the factory's hook, got %T", hook)
	}
	release()
	if !released {
		t.Fatal("resolveEventHook must return the factory's release func")
	}
}

// TestDispatchEventFansOutToGRPCResolvedHook proves DispatchEvent's factory
// branch is wired end to end: a granted, ExecGRPC entry with no in-process
// instance still receives the event, through eventHookFactory.
func TestDispatchEventFansOutToGRPCResolvedHook(t *testing.T) {
	saved := eventHookFactory
	t.Cleanup(func() { eventHookFactory = saved })

	r := NewRegistry()
	e := grpcHookEntry("hook.grpc.dispatch", CapEvents)
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := &recordingHook{}
	var released bool
	eventHookFactory = func(got *Entry) (ext.EventHook, func(), error) {
		return stub, func() { released = true }, nil
	}

	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSMkdir, Path: "/d"})

	if len(stub.got) != 1 || stub.got[0].Path != "/d" {
		t.Fatalf("gRPC-resolved hook should have received the event, got %+v", stub.got)
	}
	if !released {
		t.Fatal("DispatchEvent must call the factory's release func")
	}
}
