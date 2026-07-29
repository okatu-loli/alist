package extension

import (
	"context"
	"errors"
	"testing"

	"github.com/AlistGo/alist-ext"
)

type fakeInvoker struct {
	called   string
	gotArgs  map[string]any
	response any
}

func (f *fakeInvoker) InvokeAction(ctx context.Context, actionID string, args map[string]any) (any, error) {
	f.called = actionID
	f.gotArgs = args
	return f.response, nil
}

func actionEntry(id string, actions ...string) *Entry {
	ma := make([]ext.MenuAction, len(actions))
	for i, a := range actions {
		ma[i] = ext.MenuAction{ID: a, Label: map[string]string{"en": a}, Target: "global"}
	}
	return &Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Kinds:         []ext.Kind{ext.KindAction},
			Contributions: ext.Contributions{MenuActions: ma},
		},
		Kind:    ext.KindAction,
		Mode:    InstanceModeSingleton,
		enabled: true,
	}
}

func TestInvokeActionUndeclaredIsNotFound(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(actionEntry("ext.a", "declared"))

	// unknown extension
	if _, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.missing", "x", nil); !errors.Is(err, ErrActionNotDeclared) {
		t.Fatalf("missing extension: want ErrActionNotDeclared, got %v", err)
	}
	// known extension, undeclared action
	if _, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.a", "undeclared", nil); !errors.Is(err, ErrActionNotDeclared) {
		t.Fatalf("undeclared action: want ErrActionNotDeclared, got %v", err)
	}
}

func TestInvokeActionDeclaredInvokesRegisteredHandler(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(actionEntry("ext.a", "declared"))
	inv := &fakeInvoker{response: "ok"}
	RegisterActionInvoker("ext.a", inv)
	t.Cleanup(func() { RegisterActionInvoker("ext.a", nil) })

	res, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.a", "declared", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res != "ok" || inv.called != "declared" || inv.gotArgs["k"] != "v" {
		t.Fatalf("handler not invoked correctly: res=%v called=%q args=%v", res, inv.called, inv.gotArgs)
	}
}

func TestInvokeActionDeclaredButUnwiredIsNotImplemented(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(actionEntry("ext.a", "declared"))
	// no invoker registered
	_, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.a", "declared", nil)
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("want NOT_IMPLEMENT, got %v", err)
	}
}

func TestInvokeActionDisabledExtensionIsNotFound(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(actionEntry("ext.a", "declared"))
	_ = r.Disable("ext.a")
	if _, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.a", "declared", nil); !errors.Is(err, ErrActionNotDeclared) {
		t.Fatalf("disabled extension: want ErrActionNotDeclared, got %v", err)
	}
}

// fakeGRPCAction is a mock ext.Action standing in for a gRPC-backed executor
// resolved via actionFactory.
type fakeGRPCAction struct {
	gotPrincipal ext.Principal
	gotActionID  string
	gotArgs      map[string]any
	result       any
}

func (f *fakeGRPCAction) Manifest() ext.Manifest               { return ext.Manifest{ID: "grpc-action"} }
func (f *fakeGRPCAction) Init(context.Context, ext.Host) error { return nil }
func (f *fakeGRPCAction) Shutdown(context.Context) error       { return nil }
func (f *fakeGRPCAction) Invoke(_ context.Context, p ext.Principal, actionID string, args map[string]any) (any, error) {
	f.gotPrincipal = p
	f.gotActionID = actionID
	f.gotArgs = args
	return f.result, nil
}

// grpcActionEntry builds a declared, enabled, ExecGRPC Action entry — the
// shape resolveAction requires to even attempt the factory branch.
func grpcActionEntry(id string, actions ...string) *Entry {
	ma := make([]ext.MenuAction, len(actions))
	for i, a := range actions {
		ma[i] = ext.MenuAction{ID: a, Label: map[string]string{"en": a}, Target: "global"}
	}
	return &Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Kinds:         []ext.Kind{ext.KindAction},
			Contributions: ext.Contributions{MenuActions: ma},
		},
		Kind:     ext.KindAction,
		Mode:     InstanceModeSingleton,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-action",
	}
}

// TestInvokeActionUsesGRPCFactoryWhenNoInProcessInvoker pins the W8b seam: a
// declared action with no ActionInvoker registered, on an ExecGRPC entry with
// an installed actionFactory, is resolved and invoked through ext.Action —
// not NOT_IMPLEMENT. It also pins that the caller's principal is threaded
// through to the gRPC leg (mirrors SubmitAssist's principal-threading test).
func TestInvokeActionUsesGRPCFactoryWhenNoInProcessInvoker(t *testing.T) {
	saved := actionFactory
	t.Cleanup(func() { actionFactory = saved })

	r := NewRegistry()
	e := grpcActionEntry("ext.grpc", "declared")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	fake := &fakeGRPCAction{result: "grpc-ok"}
	var released bool
	actionFactory = func(got *Entry) (ext.Action, func(), error) {
		if got.ID() != "ext.grpc" || got.BinPath() != "/bin/example-action" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return fake, func() { released = true }, nil
	}

	res, err := InvokeExtensionAction(context.Background(), r, ext.Principal("u1"), "ext.grpc", "declared", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res != "grpc-ok" {
		t.Fatalf("result = %v, want grpc-ok", res)
	}
	if fake.gotActionID != "declared" || fake.gotArgs["k"] != "v" {
		t.Fatalf("gRPC action not invoked correctly: actionID=%q args=%v", fake.gotActionID, fake.gotArgs)
	}
	if fake.gotPrincipal != ext.Principal("u1") {
		t.Fatalf("principal = %q, want u1 threaded through to the gRPC leg", fake.gotPrincipal)
	}
	if !released {
		t.Fatal("InvokeExtensionAction must call the factory's release func")
	}
}

// TestInvokeActionInProcessInvokerTakesPriorityOverFactory pins the
// resolution order: an in-process invoker, when registered, is used even if a
// gRPC actionFactory is also installed for the same entry.
func TestInvokeActionInProcessInvokerTakesPriorityOverFactory(t *testing.T) {
	saved := actionFactory
	t.Cleanup(func() { actionFactory = saved })

	r := NewRegistry()
	e := grpcActionEntry("ext.both", "declared")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	inv := &fakeInvoker{response: "in-process-ok"}
	RegisterActionInvoker("ext.both", inv)
	t.Cleanup(func() { RegisterActionInvoker("ext.both", nil) })

	actionFactory = func(got *Entry) (ext.Action, func(), error) {
		t.Fatal("actionFactory must not be consulted when an in-process invoker is registered")
		return nil, nil, nil
	}

	res, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.both", "declared", nil)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res != "in-process-ok" || inv.called != "declared" {
		t.Fatalf("in-process invoker not used: res=%v called=%q", res, inv.called)
	}
}

// TestInvokeActionDeclaredNeitherInvokerNorFactoryIsNotImplemented re-pins
// TestInvokeActionDeclaredButUnwiredIsNotImplemented's NOT_IMPLEMENT contract
// with a gRPC actionFactory installed but returning nothing usable (nil
// factory as well as an ExecInProcess entry, which never even reaches the
// factory branch) — the resolution must still fall through to NOT_IMPLEMENT,
// not panic or silently succeed.
func TestInvokeActionDeclaredNeitherInvokerNorFactoryIsNotImplemented(t *testing.T) {
	saved := actionFactory
	t.Cleanup(func() { actionFactory = saved })
	actionFactory = nil

	r := NewRegistry()
	_ = r.Register(actionEntry("ext.none", "declared")) // ExecInProcess (zero value), no binPath

	_, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.none", "declared", nil)
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("want NOT_IMPLEMENT, got %v", err)
	}
}

// TestInvokeActionMultiKindManifestUsesManifestTruthSource pins the I1
// fixup: resolveAction must consult the Manifest's declared Kinds
// (hasKind), not Entry.HasKind, for the Action-Kind gate. A plugin entry
// registered under a different primary Kind (Kind=StorageDriver) whose
// Manifest additionally declares Action (Kinds: [StorageDriver, Action]) —
// e.g. a plugin that is both a storage driver and contributes a menu action
// — never gets an extraKinds(Action) entry (extraKinds is populated solely
// by FeatureBundle indexing, featurebundle.go), so Entry.HasKind(Action)
// would wrongly return false and 501 it. hasKind(e.Manifest, ...) must still
// let it through to actionFactory. Contrast with
// TestInvokeActionMissingKindNeverConsultsFactory immediately below, whose
// manifest omits Action entirely and must still 501.
func TestInvokeActionMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	saved := actionFactory
	t.Cleanup(func() { actionFactory = saved })

	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{
			ID:            "ext.multikind",
			Kinds:         []ext.Kind{ext.KindStorageDriver, ext.KindAction}, // declares both
			Contributions: ext.Contributions{MenuActions: []ext.MenuAction{{ID: "declared", Label: map[string]string{"en": "declared"}, Target: "global"}}},
		},
		Kind:     ext.KindStorageDriver, // registered/indexed under its primary Kind
		Mode:     InstanceModeSingleton,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-multikind",
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	fake := &fakeGRPCAction{result: "multikind-ok"}
	actionFactory = func(got *Entry) (ext.Action, func(), error) {
		if got.ID() != "ext.multikind" {
			t.Fatalf("factory received wrong entry: id=%q", got.ID())
		}
		return fake, func() {}, nil
	}

	res, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.multikind", "declared", nil)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res != "multikind-ok" {
		t.Fatalf("result = %v, want multikind-ok (actionFactory must have been consulted)", res)
	}
}

// TestInvokeActionMissingKindNeverConsultsFactory pins the Task 2 Action-Kind
// fix: an ExecGRPC entry with a binPath, whose manifest MenuActions happens
// to list the requested action id but whose declared Kinds does NOT include
// ext.KindAction (a mismatched/stale manifest), must fall straight through to
// NOT_IMPLEMENT without ever invoking actionFactory — resolveAction must not
// pay for a plugin launch attempt an undeclared Kind can never satisfy.
func TestInvokeActionMissingKindNeverConsultsFactory(t *testing.T) {
	saved := actionFactory
	t.Cleanup(func() { actionFactory = saved })

	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{
			ID:            "ext.wrongkind",
			Kinds:         []ext.Kind{ext.KindStorageDriver}, // no KindAction
			Contributions: ext.Contributions{MenuActions: []ext.MenuAction{{ID: "declared", Label: map[string]string{"en": "declared"}, Target: "global"}}},
		},
		Kind:     ext.KindStorageDriver,
		Mode:     InstanceModeSingleton,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-driver",
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	actionFactory = func(got *Entry) (ext.Action, func(), error) {
		t.Fatal("actionFactory must not be consulted for an entry that never declared Kind=Action")
		return nil, nil, nil
	}

	_, err := InvokeExtensionAction(context.Background(), r, ext.Principal(""), "ext.wrongkind", "declared", nil)
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("want NOT_IMPLEMENT, got %v", err)
	}
}
