package grpcplugin

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeAssist is the plugin-side ext.Assist used to exercise Assist over the
// wire. Mirrors fakeAction (actioninithost_test.go) one interface over.
type fakeAssist struct {
	gotPrincipal ext.Principal
	gotFormID    string
	gotInput     map[string]any
	result       any
	err          error
}

func (p *fakeAssist) Assist(ctx context.Context, principal ext.Principal, formID string, input map[string]any) (any, error) {
	p.gotPrincipal = principal
	p.gotFormID = formID
	p.gotInput = input
	return p.result, p.err
}

var _ ext.Assist = (*fakeAssist)(nil)

// fakeActionWithAssist is a Kind=Action impl that also implements ext.Assist,
// used to prove the auto-mount: ActionPluginMap(impl) must add the "assist"
// dispense name whenever impl.(ext.Assist) succeeds.
type fakeActionWithAssist struct {
	fakeAction
	fakeAssist
}

var _ ext.Action = (*fakeActionWithAssist)(nil)
var _ ext.Assist = (*fakeActionWithAssist)(nil)

// fakeAssistWithExtension is a standalone ext.Assist impl that ALSO
// implements ext.Extension — the shape a dedicated assist-only subprocess
// (Task 2's LaunchAssist) actually launches: nothing else in that process
// ever calls Init, so assistServer.InitHost must do the full reverse-host
// handshake itself and deliver a live host here.
type fakeAssistWithExtension struct {
	fakeAssist

	initCh chan ext.Host
}

func (p *fakeAssistWithExtension) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-assist-ext"} }
func (p *fakeAssistWithExtension) Shutdown(ctx context.Context) error { return nil }

func (p *fakeAssistWithExtension) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

var _ ext.Extension = (*fakeAssistWithExtension)(nil)
var _ ext.Assist = (*fakeAssistWithExtension)(nil)

// TestAssistPluginMap_StandaloneRoundTrip proves AssistPlugin/AssistPluginMap
// carries Assist across the wire intact: the principal/formID/input the host
// sent and the result the plugin returned both survive the gRPC round trip
// (JSON-encoded input_json/result_json) unmangled.
func TestAssistPluginMap_StandaloneRoundTrip(t *testing.T) {
	p := &fakeAssist{result: map[string]any{"ok": true, "count": float64(3)}}
	plugins := AssistPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AssistPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.AssistServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.AssistServiceClient", raw)
	}

	ctx := context.Background()
	inputJSON := []byte(`{"path":"/x","force":true}`)
	ar, err := svc.Assist(ctx, &pb.AssistRequest{
		Principal: "user-123",
		FormId:    "share.autocomplete",
		InputJson: inputJSON,
	})
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if e := pb.ErrorFromProto(ar.Error); e != nil {
		t.Fatalf("Assist returned error: %v", e)
	}

	if p.gotPrincipal != "user-123" {
		t.Fatalf("plugin saw principal = %q, want user-123", p.gotPrincipal)
	}
	if p.gotFormID != "share.autocomplete" {
		t.Fatalf("plugin saw formID = %q, want share.autocomplete", p.gotFormID)
	}
	if p.gotInput["path"] != "/x" || p.gotInput["force"] != true {
		t.Fatalf("plugin saw input = %+v, want path=/x force=true", p.gotInput)
	}

	if string(ar.ResultJson) != `{"count":3,"ok":true}` {
		t.Fatalf("result_json = %s, want the marshaled result map", ar.ResultJson)
	}
}

// TestAssistPluginMap_ErrorRoundTrip proves an Assist error surfaces as an
// ErrorReply rather than a transport failure or a lost result.
func TestAssistPluginMap_ErrorRoundTrip(t *testing.T) {
	p := &fakeAssist{err: ext.NewError(ext.ErrCodeObjectNotFound, "no such form")}
	plugins := AssistPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AssistPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc := raw.(pb.AssistServiceClient)

	ar, err := svc.Assist(context.Background(), &pb.AssistRequest{FormId: "share.get"})
	if err != nil {
		t.Fatalf("Assist transport error: %v", err)
	}
	got := pb.ErrorFromProto(ar.Error)
	if got == nil {
		t.Fatal("want a business error, got nil")
	}
	code, ok := ext.CodeOf(got)
	if !ok || code != ext.ErrCodeObjectNotFound {
		t.Fatalf("error code = %v (ok=%v), want ErrCodeObjectNotFound", code, ok)
	}
}

// TestAssistPluginMap_InitHost_NoExtension_NoOp proves that an ext.Assist
// impl which does NOT also implement ext.Extension still dispenses cleanly
// even with a HostServerFactory wired: there is nothing to Init, so
// assistServer.InitHost must not error or block, just skip it.
func TestAssistPluginMap_InitHost_NoExtension_NoOp(t *testing.T) {
	p := &fakeAssist{}
	plugins := map[string]plugin.Plugin{
		pb.AssistPluginName: &AssistPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AssistPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AssistServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AssistServiceClient", raw)
	}
}

// TestAssistPluginMap_InitHost_WithExtension_InitsHost proves the Major fix
// (review round 1, plan b): a dedicated assist-only subprocess never gets its
// impl Init'd through any other dispense, so assistServer.InitHost must do
// the reverse-host handshake itself and call impl.Init with a live,
// non-nil host whenever impl also implements ext.Extension. Mirrors
// TestActionInitHost_WithHostServerFactory (actioninithost_test.go) one Kind
// over.
func TestAssistPluginMap_InitHost_WithExtension_InitsHost(t *testing.T) {
	p := &fakeAssistWithExtension{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.AssistPluginName: &AssistPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AssistPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.AssistServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.AssistServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		if h == nil {
			t.Fatal("Init called with nil host")
		}
		cfg, err := h.LoadConfig(context.Background())
		if err != nil {
			t.Fatalf("host.LoadConfig: %v", err)
		}
		if string(cfg) != `{"hello":"host"}` {
			t.Fatalf("host.LoadConfig = %s, want the fakeHostServer payload", cfg)
		}
	default:
		t.Fatal("Init was not called synchronously by Dispense/InitHost")
	}
}

// TestActionPluginMap_AutoMountsAssist proves the "自动挂载" contract: when a
// Kind=Action impl also implements ext.Assist, ActionPluginMap(impl) must add
// the "assist" dispense name for the same underlying value — the assist
// dispense's Assist calls reach the very same impl.
func TestActionPluginMap_AutoMountsAssist(t *testing.T) {
	p := &fakeActionWithAssist{}
	p.fakeAssist.result = "echoed"
	plugins := ActionPluginMap(p)

	if _, ok := plugins[pb.ActionPluginName]; !ok {
		t.Fatal("ActionPluginMap did not include the action dispense name")
	}
	assistPlugin, ok := plugins[pb.AssistPluginName]
	if !ok {
		t.Fatal("ActionPluginMap did not auto-mount the assist dispense name for an impl implementing ext.Assist")
	}
	ap, ok := assistPlugin.(*AssistPlugin)
	if !ok {
		t.Fatalf("plugins[assist] is %T, want *AssistPlugin", assistPlugin)
	}
	if ap.Impl != ext.Assist(p) {
		t.Fatal("AssistPlugin.Impl is not the same underlying value as the Action impl")
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.AssistPluginName)
	if err != nil {
		t.Fatalf("Dispense(assist): %v", err)
	}
	svc := raw.(pb.AssistServiceClient)
	ar, err := svc.Assist(context.Background(), &pb.AssistRequest{FormId: "f"})
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if e := pb.ErrorFromProto(ar.Error); e != nil {
		t.Fatalf("Assist error: %v", e)
	}
	if string(ar.ResultJson) != `"echoed"` {
		t.Fatalf("result_json = %s, want %q", ar.ResultJson, `"echoed"`)
	}
}

// TestActionPluginMap_NoAssist_NoAutoMount proves an impl that does NOT
// implement ext.Assist gets no "assist" dispense name (old-plugin
// compatibility: behavior is unchanged for plugins that never opted in).
func TestActionPluginMap_NoAssist_NoAutoMount(t *testing.T) {
	p := &fakeAction{initCh: make(chan ext.Host, 1)}
	plugins := ActionPluginMap(p)

	if _, ok := plugins[pb.AssistPluginName]; ok {
		t.Fatal("ActionPluginMap auto-mounted assist for an impl that does not implement ext.Assist")
	}
}

// TestAssistDispense_NotMounted_Fails proves that dispensing "assist" from a
// plugin set whose impl never implemented ext.Assist fails cleanly (the host
// boundary maps this dispense failure to ErrCodeNotImplement — Task 2), not
// silently.
func TestAssistDispense_NotMounted_Fails(t *testing.T) {
	p := &fakeAction{initCh: make(chan ext.Host, 1)}
	plugins := ActionPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	// Only assert that Dispense fails: the exact wording ("unknown plugin
	// type: ...") is go-plugin's own (hashicorp/go-plugin, grpc_client.go),
	// not alist's — pinning it here would couple this test to a dependency
	// version's error string.
	if _, err := client.Dispense(pb.AssistPluginName); err == nil {
		t.Fatal("Dispense(assist) succeeded, want an error since impl never implemented ext.Assist")
	}
}
