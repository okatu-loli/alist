package extension

import (
	"context"
	"errors"
	"testing"

	"github.com/AlistGo/alist-ext"
)

type fakeAssistProvider struct {
	called   string
	gotInput map[string]any
	response any
}

func (f *fakeAssistProvider) Assist(ctx context.Context, formID string, input map[string]any) (any, error) {
	f.called = formID
	f.gotInput = input
	return f.response, nil
}

func assistEntry(id string, forms ...string) *Entry {
	af := make([]ext.AssistForm, len(forms))
	for i, f := range forms {
		af[i] = ext.AssistForm{ID: f, Title: map[string]string{"en": f}}
	}
	return &Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Kinds:         []ext.Kind{ext.KindAction},
			Contributions: ext.Contributions{AssistForms: af},
		},
		Kind:    ext.KindAction,
		Mode:    InstanceModeSingleton,
		enabled: true,
	}
}

func TestSubmitAssistUndeclaredIsNotFound(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(assistEntry("ext.a", "declared"))

	// unknown extension
	if _, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.missing", "x", nil); !errors.Is(err, ErrAssistNotDeclared) {
		t.Fatalf("missing extension: want ErrAssistNotDeclared, got %v", err)
	}
	// known extension, undeclared form
	if _, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.a", "undeclared", nil); !errors.Is(err, ErrAssistNotDeclared) {
		t.Fatalf("undeclared form: want ErrAssistNotDeclared, got %v", err)
	}
}

// assistEntryWithSchema builds a declared, enabled, ExecInProcess entry whose
// single assist form carries schema, mirroring assistEntry but for the
// schema-validation tests (SP-H "纵深" hardening item 3).
func assistEntryWithSchema(id, formID string, schema *ext.JSONSchema) *Entry {
	return &Entry{
		Manifest: ext.Manifest{
			ID:    id,
			Kinds: []ext.Kind{ext.KindAction},
			Contributions: ext.Contributions{
				AssistForms: []ext.AssistForm{{ID: formID, Title: map[string]string{"en": formID}, Schema: schema}},
			},
		},
		Kind:    ext.KindAction,
		Mode:    InstanceModeSingleton,
		enabled: true,
	}
}

// TestSubmitAssistSchemaRequiredFieldMissingIsRejected pins item 3: a
// declared AssistForm.Schema is enforced at the host boundary — a submission
// missing a required field never reaches the provider at all, and the error
// is ErrAssistInvalidInput (the handler maps this to 400).
func TestSubmitAssistSchemaRequiredFieldMissingIsRejected(t *testing.T) {
	r := NewRegistry()
	schema := &ext.JSONSchema{
		Type:       "object",
		Properties: map[string]*ext.JSONSchema{"email": {Type: "string"}},
		Required:   []string{"email"},
	}
	_ = r.Register(assistEntryWithSchema("ext.schema", "declared", schema))
	p := &fakeAssistProvider{response: "ok"}
	RegisterAssistProvider("ext.schema", p)
	t.Cleanup(func() { RegisterAssistProvider("ext.schema", nil) })

	_, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.schema", "declared", map[string]any{})
	if !errors.Is(err, ErrAssistInvalidInput) {
		t.Fatalf("want ErrAssistInvalidInput, got %v", err)
	}
	if p.called != "" {
		t.Fatal("provider must not be invoked when schema validation fails")
	}
}

// TestSubmitAssistSchemaValidInputReachesProvider pins the happy path: input
// satisfying the declared schema still reaches the provider unchanged.
func TestSubmitAssistSchemaValidInputReachesProvider(t *testing.T) {
	r := NewRegistry()
	schema := &ext.JSONSchema{
		Type:       "object",
		Properties: map[string]*ext.JSONSchema{"email": {Type: "string"}},
		Required:   []string{"email"},
	}
	_ = r.Register(assistEntryWithSchema("ext.schema2", "declared", schema))
	p := &fakeAssistProvider{response: "ok"}
	RegisterAssistProvider("ext.schema2", p)
	t.Cleanup(func() { RegisterAssistProvider("ext.schema2", nil) })

	res, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.schema2", "declared", map[string]any{"email": "a@b.com"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res != "ok" || p.called != "declared" {
		t.Fatalf("provider not invoked: res=%v called=%q", res, p.called)
	}
}

// TestSubmitAssistNoSchemaFormIsUnaffected pins the "nil schema unchanged"
// half: a form declaring no Schema is not subject to any validation, exactly
// as before this hardening (assistEntry — every other test in this file —
// builds forms with a nil Schema).
func TestSubmitAssistNoSchemaFormIsUnaffected(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(assistEntry("ext.noschema", "declared"))
	p := &fakeAssistProvider{response: "ok"}
	RegisterAssistProvider("ext.noschema", p)
	t.Cleanup(func() { RegisterAssistProvider("ext.noschema", nil) })

	res, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.noschema", "declared", map[string]any{})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res != "ok" {
		t.Fatalf("res = %v, want ok", res)
	}
}

func TestSubmitAssistDeclaredInvokesRegisteredProvider(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(assistEntry("ext.a", "declared"))
	p := &fakeAssistProvider{response: "ok"}
	RegisterAssistProvider("ext.a", p)
	t.Cleanup(func() { RegisterAssistProvider("ext.a", nil) })

	res, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.a", "declared", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res != "ok" || p.called != "declared" || p.gotInput["k"] != "v" {
		t.Fatalf("provider not invoked correctly: res=%v called=%q input=%v", res, p.called, p.gotInput)
	}
}

func TestSubmitAssistDeclaredButUnwiredIsNotImplemented(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(assistEntry("ext.a", "declared"))
	// no provider registered
	_, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.a", "declared", nil)
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("want NOT_IMPLEMENT, got %v", err)
	}
}

func TestSubmitAssistDisabledExtensionIsNotFound(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(assistEntry("ext.a", "declared"))
	_ = r.Disable("ext.a")
	if _, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.a", "declared", nil); !errors.Is(err, ErrAssistNotDeclared) {
		t.Fatalf("disabled extension: want ErrAssistNotDeclared, got %v", err)
	}
}

// fakeGRPCAssist is a mock ext.Assist standing in for a gRPC-backed executor
// resolved via assistFactory.
type fakeGRPCAssist struct {
	gotPrincipal ext.Principal
	gotFormID    string
	gotInput     map[string]any
	result       any
}

func (f *fakeGRPCAssist) Assist(_ context.Context, p ext.Principal, formID string, input map[string]any) (any, error) {
	f.gotPrincipal = p
	f.gotFormID = formID
	f.gotInput = input
	return f.result, nil
}

// grpcAssistEntry builds a declared, enabled, ExecGRPC assist-form entry —
// the shape resolveAssist requires to even attempt the factory branch. Its
// Kind is deliberately StorageDriver, not Action: resolveAssist must not
// require Kind=Action the way resolveAction does — an assist form can be
// declared by any Kind (a StorageDriver's config wizard, ...).
func grpcAssistEntry(id string, forms ...string) *Entry {
	af := make([]ext.AssistForm, len(forms))
	for i, f := range forms {
		af[i] = ext.AssistForm{ID: f, Title: map[string]string{"en": f}}
	}
	return &Entry{
		Manifest: ext.Manifest{
			ID:            id,
			Kinds:         []ext.Kind{ext.KindStorageDriver},
			Contributions: ext.Contributions{AssistForms: af},
		},
		Kind:     ext.KindStorageDriver,
		Mode:     InstanceModeSingleton,
		enabled:  true,
		execMode: ExecGRPC,
		binPath:  "/bin/example-assist",
	}
}

// TestSubmitAssistUsesGRPCFactoryWhenNoInProcessProvider pins the Task 2
// seam: a declared assist form with no AssistProvider registered, on an
// ExecGRPC entry with an installed assistFactory, is resolved and invoked
// through ext.Assist — not NOT_IMPLEMENT. It also pins that the caller's
// principal is threaded through to the gRPC leg (unlike the in-process
// AssistProvider seam, which never saw one).
func TestSubmitAssistUsesGRPCFactoryWhenNoInProcessProvider(t *testing.T) {
	saved := assistFactory
	t.Cleanup(func() { assistFactory = saved })

	r := NewRegistry()
	e := grpcAssistEntry("ext.grpc", "declared")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	fake := &fakeGRPCAssist{result: "grpc-ok"}
	var released bool
	assistFactory = func(got *Entry) (ext.Assist, func(), error) {
		if got.ID() != "ext.grpc" || got.BinPath() != "/bin/example-assist" {
			t.Fatalf("factory received wrong entry: id=%q bin=%q", got.ID(), got.BinPath())
		}
		return fake, func() { released = true }, nil
	}

	res, err := SubmitAssist(context.Background(), r, ext.Principal("u1"), "ext.grpc", "declared", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res != "grpc-ok" {
		t.Fatalf("result = %v, want grpc-ok", res)
	}
	if fake.gotFormID != "declared" || fake.gotInput["k"] != "v" {
		t.Fatalf("gRPC assist not invoked correctly: formID=%q input=%v", fake.gotFormID, fake.gotInput)
	}
	if fake.gotPrincipal != ext.Principal("u1") {
		t.Fatalf("principal = %q, want u1 threaded through to the gRPC leg", fake.gotPrincipal)
	}
	if !released {
		t.Fatal("SubmitAssist must call the factory's release func")
	}
}

// TestSubmitAssistInProcessProviderTakesPriorityOverFactory pins the
// resolution order: an in-process provider, when registered, is used even if
// a gRPC assistFactory is also installed for the same entry.
func TestSubmitAssistInProcessProviderTakesPriorityOverFactory(t *testing.T) {
	saved := assistFactory
	t.Cleanup(func() { assistFactory = saved })

	r := NewRegistry()
	e := grpcAssistEntry("ext.both", "declared")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	p := &fakeAssistProvider{response: "in-process-ok"}
	RegisterAssistProvider("ext.both", p)
	t.Cleanup(func() { RegisterAssistProvider("ext.both", nil) })

	assistFactory = func(got *Entry) (ext.Assist, func(), error) {
		t.Fatal("assistFactory must not be consulted when an in-process provider is registered")
		return nil, nil, nil
	}

	res, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.both", "declared", nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res != "in-process-ok" || p.called != "declared" {
		t.Fatalf("in-process provider not used: res=%v called=%q", res, p.called)
	}
}

// TestSubmitAssistDeclaredNeitherProviderNorFactoryIsNotImplemented re-pins
// TestSubmitAssistDeclaredButUnwiredIsNotImplemented's NOT_IMPLEMENT contract
// with a gRPC assistFactory installed but never reached (an ExecInProcess
// entry never even attempts the factory branch) — the resolution must still
// fall through to NOT_IMPLEMENT, not panic or silently succeed.
func TestSubmitAssistDeclaredNeitherProviderNorFactoryIsNotImplemented(t *testing.T) {
	saved := assistFactory
	t.Cleanup(func() { assistFactory = saved })
	assistFactory = nil

	r := NewRegistry()
	_ = r.Register(assistEntry("ext.none", "declared")) // ExecInProcess (zero value), no binPath

	_, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.none", "declared", nil)
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("want NOT_IMPLEMENT, got %v", err)
	}
}

// TestSubmitAssistGRPCFactoryErrorFallsThroughToNotImplemented pins the
// dispense-failure compatibility path (host.LaunchAssist / assistclient.go):
// when the factory itself errors (mirrors a plugin that never mounted
// "assist"), resolveAssist must treat that as "nothing resolved", not
// propagate the factory's raw error — SubmitAssist still returns the seam's
// own NOT_IMPLEMENT.
func TestSubmitAssistGRPCFactoryErrorFallsThroughToNotImplemented(t *testing.T) {
	saved := assistFactory
	t.Cleanup(func() { assistFactory = saved })

	r := NewRegistry()
	e := grpcAssistEntry("ext.grpcerr", "declared")
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}

	assistFactory = func(got *Entry) (ext.Assist, func(), error) {
		return nil, nil, ext.NewError(ext.ErrCodeNotImplement, "plugin does not implement assist")
	}

	_, err := SubmitAssist(context.Background(), r, ext.Principal(""), "ext.grpcerr", "declared", nil)
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("want NOT_IMPLEMENT, got %v", err)
	}
}
