package extension

import (
	"context"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// stubDecorator provides one group; it can be made slow to exceed the budget.
type stubDecorator struct {
	groups []string
	sleep  time.Duration
	value  string
}

func (s *stubDecorator) Manifest() ext.Manifest               { return ext.Manifest{} }
func (s *stubDecorator) Init(context.Context, ext.Host) error { return nil }
func (s *stubDecorator) Shutdown(context.Context) error       { return nil }
func (s *stubDecorator) Groups() []string                     { return s.groups }
func (s *stubDecorator) Decorate(ctx context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	if s.sleep > 0 {
		select {
		case <-time.After(s.sleep):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := map[string]any{}
	for _, ref := range refs {
		out[ext.RefKey(ref)] = s.value
	}
	return out, nil
}

func decoratorEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindFileDecorator}},
		Kind:     ext.KindFileDecorator,
		enabled:  true,
	}
}

// grpcDecoratorEntry builds a declared, enabled, ExecGRPC Kind=FileDecorator
// entry — the shape resolveFileDecorator requires to even attempt the
// factory branch. Mirrors grpcHookEntry (eventhook_test.go) one Kind over.
func grpcDecoratorEntry(id string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindFileDecorator}},
		Kind:     ext.KindFileDecorator,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-filedecorator",
	}
}

// TestResolveFileDecoratorUsesFactory pins the gRPC-dispatch seam: an
// ExecGRPC entry with a binPath and no in-process instance must resolve
// through the installed fileDecoratorFactory (the plugins.go init wires this
// to pluginproc.Default.AcquireFileDecorator). Without a factory the same
// entry resolves to nil (no host-side handler). Mirrors
// TestResolveEventHookUsesFactory one Kind over.
func TestResolveFileDecoratorUsesFactory(t *testing.T) {
	saved := fileDecoratorFactory
	t.Cleanup(func() { fileDecoratorFactory = saved })

	e := grpcDecoratorEntry("dec.grpc")

	// No factory wired → no host-side handler → nil.
	fileDecoratorFactory = nil
	if dec, _ := resolveFileDecorator(e); dec != nil {
		t.Fatalf("without a factory, an ExecGRPC entry must resolve to nil, got %T", dec)
	}

	// Factory wired → the ExecGRPC branch is hit and its decorator is returned.
	stub := &stubDecorator{groups: []string{"_labels"}, value: "grpc-value"}
	var released bool
	fileDecoratorFactory = func(got *Entry) (ext.FileDecorator, func(), error) {
		if got.ID() != "dec.grpc" || got.BinPath() != "/bin/example-filedecorator" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return stub, func() { released = true }, nil
	}
	dec, release := resolveFileDecorator(e)
	if dec != stub {
		t.Fatalf("resolveFileDecorator must return the factory's decorator, got %T", dec)
	}
	release()
	if !released {
		t.Fatal("resolveFileDecorator must return the factory's release func")
	}
}

// TestDecorateFieldsUsesGRPCResolvedDecorator proves DecorateFields' factory
// branch is wired end to end: a granted, ExecGRPC entry with no in-process
// instance still contributes its group, through fileDecoratorFactory.
func TestDecorateFieldsUsesGRPCResolvedDecorator(t *testing.T) {
	saved := fileDecoratorFactory
	t.Cleanup(func() { fileDecoratorFactory = saved })

	r := NewRegistry()
	e := grpcDecoratorEntry("dec.grpc.dispatch")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	stub := &stubDecorator{groups: []string{"_labels"}, value: "grpc-value"}
	var released bool
	fileDecoratorFactory = func(got *Entry) (ext.FileDecorator, func(), error) {
		return stub, func() { released = true }, nil
	}

	refs := []ext.ObjRef{{Anchor: "/x"}}
	res := DecorateFields(context.Background(), r, []string{"_labels"}, refs, 200*time.Millisecond)

	if res.Fields["_labels"] == nil || res.Fields["_labels"]["/x"] != "grpc-value" {
		t.Fatalf("gRPC-resolved decorator should have contributed its group, got %+v", res.Fields)
	}
	if !released {
		t.Fatal("DecorateFields must call the factory's release func")
	}
}

func TestDecorateFieldsFastGroupPopulated(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(decoratorEntry("dec.labels")); err != nil {
		t.Fatal(err)
	}
	RegisterFileDecoratorInstance("dec.labels", &stubDecorator{groups: []string{"_labels"}, value: "red"})
	t.Cleanup(func() { RegisterFileDecoratorInstance("dec.labels", nil) })

	refs := []ext.ObjRef{{Anchor: "/dir/a.txt"}, {Anchor: "/dir/b.txt"}}
	res := DecorateFields(context.Background(), r, []string{"_labels"}, refs, 200*time.Millisecond)

	if len(res.Pending) != 0 {
		t.Fatalf("fast group must not be pending: %v", res.Pending)
	}
	grp := res.Fields["_labels"]
	if grp == nil || grp["/dir/a.txt"] != "red" || grp["/dir/b.txt"] != "red" {
		t.Fatalf("labels group not populated: %+v", res.Fields)
	}
}

// A decorator slower than the budget marks ITS group pending without blocking
// past the budget, and a co-requested fast group still returns.
func TestDecorateFieldsSlowGroupMarkedPending(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(decoratorEntry("dec.slow")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(decoratorEntry("dec.fast")); err != nil {
		t.Fatal(err)
	}
	RegisterFileDecoratorInstance("dec.slow", &stubDecorator{groups: []string{"_shares"}, sleep: 500 * time.Millisecond, value: "shared"})
	RegisterFileDecoratorInstance("dec.fast", &stubDecorator{groups: []string{"_labels"}, value: "blue"})
	t.Cleanup(func() {
		RegisterFileDecoratorInstance("dec.slow", nil)
		RegisterFileDecoratorInstance("dec.fast", nil)
	})

	refs := []ext.ObjRef{{Anchor: "/x"}}
	start := time.Now()
	res := DecorateFields(context.Background(), r, []string{"_labels", "_shares"}, refs, 50*time.Millisecond)
	elapsed := time.Since(start)

	// The call returned near the budget, not the slow decorator's 500ms.
	if elapsed > 300*time.Millisecond {
		t.Fatalf("DecorateFields blocked on the slow decorator: took %s", elapsed)
	}
	if len(res.Pending) != 1 || res.Pending[0] != "_shares" {
		t.Fatalf("slow group must be pending: %v", res.Pending)
	}
	if res.Fields["_labels"] == nil || res.Fields["_labels"]["/x"] != "blue" {
		t.Fatalf("fast group must still return alongside a pending one: %+v", res.Fields)
	}
	if _, ok := res.Fields["_shares"]; ok {
		t.Fatalf("pending group must not appear in Fields: %+v", res.Fields)
	}
}

// A group no enabled decorator provides is silently empty (not pending — there
// is no one to backfill from).
func TestDecorateFieldsUnknownGroupNotPending(t *testing.T) {
	r := NewRegistry()
	res := DecorateFields(context.Background(), r, []string{"_nobody"}, []ext.ObjRef{{Anchor: "/x"}}, 50*time.Millisecond)
	if len(res.Pending) != 0 || len(res.Fields) != 0 {
		t.Fatalf("unknown group: want empty result, got %+v", res)
	}
}

// A decorator that panics fails its group closed (pending), not the process.
func TestDecorateFieldsPanicIsolated(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(decoratorEntry("dec.panic")); err != nil {
		t.Fatal(err)
	}
	RegisterFileDecoratorInstance("dec.panic", panicDecorator{})
	t.Cleanup(func() { RegisterFileDecoratorInstance("dec.panic", nil) })

	res := DecorateFields(context.Background(), r, []string{"_boom"}, []ext.ObjRef{{Anchor: "/x"}}, 50*time.Millisecond)
	if len(res.Pending) != 1 || res.Pending[0] != "_boom" {
		t.Fatalf("panicking decorator must mark its group pending: %v", res.Pending)
	}
}

type panicDecorator struct{}

func (panicDecorator) Manifest() ext.Manifest               { return ext.Manifest{} }
func (panicDecorator) Init(context.Context, ext.Host) error { return nil }
func (panicDecorator) Shutdown(context.Context) error       { return nil }
func (panicDecorator) Groups() []string                     { return []string{"_boom"} }
func (panicDecorator) Decorate(context.Context, string, []ext.ObjRef) (map[string]any, error) {
	panic("decorator boom")
}
