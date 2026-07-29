package grpc

import (
	"context"
	"os/exec"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
)

// TestAssistGRPCRoundTrip is the assist gRPC leg's Task 3 round-trip gate:
// drive Assist against a real go-plugin subprocess (the actionplugin fixture,
// which also implements ext.Assist) and confirm the auto-mounted "assist"
// dispense name carries principal/formID/input across the wire intact.
// Mirrors TestActionGRPCRoundTrip (actionparity_test.go) one service over.
func TestAssistGRPCRoundTrip(t *testing.T) {
	c := launchAssistPlugin(t)
	ctx := context.Background()

	res, err := c.Assist(ctx, "principal-x", "probe", map[string]any{"q": "1"})
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("Assist result type = %T, want map[string]any", res)
	}
	if m["form"] != "probe" {
		t.Fatalf("Assist result form = %+v, want probe", m["form"])
	}
	echo, ok := m["echo"].(map[string]any)
	if !ok || echo["q"] != "1" {
		t.Fatalf("Assist result echo = %+v", m["echo"])
	}
}

// launchAssistPlugin starts the actionplugin fixture (which also implements
// ext.Assist) and dispenses its dedicated assist auxiliary service, mirroring
// how pluginproc.Manager.AcquireAssist launches a separate assist-only
// subprocess in production.
func launchAssistPlugin(t *testing.T) *host.AssistClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchAssist(context.Background(), exec.Command(actionPluginBin), nil)
	if err != nil {
		t.Fatalf("launch assist plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// TestAssistGRPCThroughSubmitAssist drives the full host-side path this Task
// 3 delivers: extension.SetAssistFactory -> extension.SubmitAssist -> a real
// gRPC subprocess's Assist, exactly the wiring server/handles/plugins.go
// installs in production (with pluginproc.Default.AcquireAssist in place of
// this test's direct host.LaunchAssist). Mirrors
// TestActionGRPCDispatchThroughExtension one service over.
func TestAssistGRPCThroughSubmitAssist(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchAssistPlugin(t)

	extension.SetAssistFactory(func(e *extension.Entry) (ext.Assist, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetAssistFactory(nil) })

	r := extension.NewRegistry()
	m := ext.Manifest{
		ID:    "com.example.testaction",
		Kinds: []ext.Kind{ext.KindAction},
		Contributions: ext.Contributions{
			AssistForms: []ext.AssistForm{{ID: "probe", Title: map[string]string{"en": "Probe"}}},
		},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	res, err := extension.SubmitAssist(context.Background(), r, "", "com.example.testaction", "probe",
		map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("SubmitAssist: %v", err)
	}
	got, ok := res.(map[string]any)
	if !ok || got["form"] != "probe" {
		t.Fatalf("SubmitAssist result = %+v", res)
	}
	echo, ok := got["echo"].(map[string]any)
	if !ok || echo["k"] != "v" {
		t.Fatalf("SubmitAssist result echo = %+v", got["echo"])
	}
}

// TestAssistGRPCNotMountedMapsToNotImplement covers the T2 review gap for
// the hostBuilder==nil call shape (no reverse-host bridge): a plugin binary
// whose impl never implements ext.Assist (the urlplugin fixture — a bare
// ext.StorageDriver) never registers an AssistServiceServer on its shared
// gRPC server. With hostBuilder nil, LaunchAssist's Dispense itself still
// succeeds — go-plugin's gRPC Dispense (grpc_client.go) is a purely local
// client-side lookup against the host's OWN plugin set
// (grpcplugin.AssistPluginMap(nil) always carries the "assist" key), and
// AssistPlugin.GRPCClient skips the InitHost round trip entirely when
// HostServerFactory is nil, so nothing asks the peer whether it actually
// serves the service at Dispense time. The peer's absence only surfaces once
// an RPC is actually issued: the shared grpc.Server replies
// codes.Unimplemented for the unregistered AssistService, which
// AssistClient.Assist must map to ext.ErrCodeNotImplement — the T2 review
// point this test exists to close (old plugin without an assist leg -> 501,
// not a raw transport error).
//
// This is NOT the only path to ErrCodeNotImplement for such a plugin: when
// hostBuilder is non-nil (the production shape — see
// TestAssistGRPCNotMountedWithHostBuilderMapsToNotImplement below), the
// InitHost round trip that AssistPlugin.GRPCClient makes during Dispense
// itself hits the same unregistered-service wall and fails right there, so
// LaunchAssist's own Dispense-error branch is the one that maps to
// ErrCodeNotImplement instead. Neither branch is dead code; each is reached
// by a different, real call shape.
func TestAssistGRPCNotMountedMapsToNotImplement(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	ctx := context.Background()
	c, err := host.LaunchAssist(ctx, exec.Command(urlPluginBin), nil)
	if err != nil {
		t.Fatalf("LaunchAssist: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(ctx) })

	_, err = c.Assist(ctx, "", "probe", nil)
	if err == nil {
		t.Fatal("Assist against a plugin with no ext.Assist impl succeeded, want an error")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("Assist error = %v (code %q, ok=%v), want ErrCodeNotImplement", err, code, ok)
	}
}

// TestAssistGRPCNotMountedWithHostBuilderMapsToNotImplement is the I2 fixup:
// it covers the hostBuilder != nil call shape, which is the production
// shape — pluginproc.Manager.AcquireAssist always launches via
// m.launchHostBuilder(pluginID) (manager.go), which is non-nil in production
// once internal/bootstrap wires SetHostBuilder at boot (mirrors
// TestReverseBridgeProductionPath's wiring, hostcall_integration_test.go,
// one Kind over).
//
// With a non-nil hostBuilder, AssistPlugin.GRPCClient (ext/grpcplugin/
// assistserve.go) issues an InitHost RPC to the peer as part of the gRPC
// Dispense call itself (go-plugin's GRPCClient.Dispense returns
// p.GRPCClient(...) directly — grpc_client.go). The urlplugin fixture never
// implements ext.Assist, so its Serve-side plugin set never mounted
// AssistServiceServer (mountAssist only runs for an impl that is an
// ext.Assist); the peer's shared grpc.Server therefore answers
// codes.Unimplemented to InitHost, GRPCClient returns that as an error,
// Dispense propagates it, and LaunchAssist's own error-handling branch — the
// one wrapping a Dispense failure as ext.ErrCodeNotImplement — is what
// fires. That branch is the production main fallback path for an
// old/partial plugin under the reverse-host bridge, not unreachable
// "defensive" code: unlike the hostBuilder==nil test above, here the error
// surfaces from LaunchAssist itself, before c.Assist is ever called.
func TestAssistGRPCNotMountedWithHostBuilderMapsToNotImplement(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	ctx := context.Background()
	hostBuilder := func() ext.Host {
		return hostcap.New(hostcap.Grant{PluginID: "grpc-notmounted-hostbuilder-test"}, hostcap.Options{})
	}
	_, err := host.LaunchAssist(ctx, exec.Command(urlPluginBin), hostBuilder)
	if err == nil {
		t.Fatal("LaunchAssist with a hostBuilder against a plugin with no ext.Assist impl succeeded, want an error")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("LaunchAssist error = %v (code %q, ok=%v), want ErrCodeNotImplement", err, code, ok)
	}
}
