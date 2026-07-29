package grpcplugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeNotification is the plugin-side ext.Notification used to observe
// whether InitHost delivered a working host to Init, and to exercise Notify
// over the wire. Mirrors fakeEventHook (eventhookinithost_test.go) one Kind
// over.
type fakeNotification struct {
	initCh chan ext.Host

	gotNotice ext.Notice
	err       error
}

func (p *fakeNotification) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-notification"} }
func (p *fakeNotification) Shutdown(ctx context.Context) error { return nil }

func (p *fakeNotification) Init(ctx context.Context, host ext.Host) error {
	p.initCh <- host
	return nil
}

func (p *fakeNotification) Notify(ctx context.Context, n ext.Notice) error {
	p.gotNotice = n
	return p.err
}

var _ ext.Notification = (*fakeNotification)(nil)

// failingInitNotification is a Notification whose Init always fails, used to
// prove that a business-error ErrorReply from notificationServer.InitHost (as
// opposed to a gRPC transport error) still makes GRPCClient — and therefore
// Dispense — fail loudly. Mirrors failingInitEventHook.
type failingInitNotification struct{}

func (p *failingInitNotification) Manifest() ext.Manifest {
	return ext.Manifest{ID: "failing-init-notification"}
}
func (p *failingInitNotification) Shutdown(ctx context.Context) error { return nil }

func (p *failingInitNotification) Init(ctx context.Context, host ext.Host) error {
	return errors.New("boom")
}

func (p *failingInitNotification) Notify(ctx context.Context, n ext.Notice) error { return nil }

var _ ext.Notification = (*failingInitNotification)(nil)

// TestNotificationInitHost_WithHostServerFactory mirrors
// TestEventHookInitHost_WithHostServerFactory one Kind over: it proves the
// full handshake end to end using go-plugin's in-process test harness.
func TestNotificationInitHost_WithHostServerFactory(t *testing.T) {
	p := &fakeNotification{initCh: make(chan ext.Host, 1)}
	plugins := map[string]plugin.Plugin{
		pb.NotificationPluginName: &NotificationPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.NotificationPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.NotificationServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.NotificationServiceClient", raw)
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

// TestNotificationInitHost_NilFactory_InitNotCalled mirrors
// TestEventHookInitHost_NilFactory_InitNotCalled: with HostServerFactory nil
// (NotificationPluginMap, as before this feature existed), GRPCClient never
// calls InitHost and the channel's Init is never invoked.
func TestNotificationInitHost_NilFactory_InitNotCalled(t *testing.T) {
	p := &fakeNotification{initCh: make(chan ext.Host, 1)}
	plugins := NotificationPluginMap(p) // HostServerFactory left nil

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.NotificationPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	if _, ok := raw.(pb.NotificationServiceClient); !ok {
		t.Fatalf("Dispense returned %T, want pb.NotificationServiceClient", raw)
	}

	select {
	case h := <-p.initCh:
		t.Fatalf("Init was called with host %v, want not called (nil factory = old behavior)", h)
	default:
		// expected: nothing arrived
	}
}

// TestNotificationInitHost_InitFails_DispenseErrors mirrors
// TestEventHookInitHost_InitFails_DispenseErrors: a channel Init failure must
// fail the Dispense, not silently hand back a live client.
func TestNotificationInitHost_InitFails_DispenseErrors(t *testing.T) {
	p := &failingInitNotification{}
	plugins := map[string]plugin.Plugin{
		pb.NotificationPluginName: &NotificationPlugin{Impl: p, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.NotificationPluginName)
	if err == nil {
		t.Fatalf("Dispense succeeded with raw=%v, want an error propagating the channel's Init failure", raw)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Dispense error = %q, want it to contain the channel's Init error (%q)", err.Error(), "boom")
	}
}

// TestNotificationNotifyRoundTrip proves Notify crosses the wire intact:
// every ext.Notice field (level/title/body/source/unix_milli) the host sent
// survives the gRPC round trip unmangled.
func TestNotificationNotifyRoundTrip(t *testing.T) {
	p := &fakeNotification{initCh: make(chan ext.Host, 1)}
	plugins := NotificationPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.NotificationPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc, ok := raw.(pb.NotificationServiceClient)
	if !ok {
		t.Fatalf("Dispense returned %T, want pb.NotificationServiceClient", raw)
	}

	ctx := context.Background()
	reply, err := svc.Notify(ctx, &pb.NoticeProto{
		Level:     string(ext.NoticeWarn),
		Title:     "storage offline",
		Body:      "the mount stopped responding",
		Source:    "storage",
		UnixMilli: 1234567,
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		t.Fatalf("Notify returned error: %v", e)
	}

	want := ext.Notice{
		Level:     ext.NoticeWarn,
		Title:     "storage offline",
		Body:      "the mount stopped responding",
		Source:    "storage",
		UnixMilli: 1234567,
	}
	if p.gotNotice != want {
		t.Fatalf("plugin saw notice = %+v, want %+v", p.gotNotice, want)
	}
}

// TestNotificationNotifyErrorRoundTrip proves a Notify error surfaces as an
// ErrorReply rather than a transport failure.
func TestNotificationNotifyErrorRoundTrip(t *testing.T) {
	p := &fakeNotification{
		initCh: make(chan ext.Host, 1),
		err:    ext.NewError(ext.ErrCodeObjectNotFound, "no such channel"),
	}
	plugins := NotificationPluginMap(p)

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	raw, err := client.Dispense(pb.NotificationPluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	svc := raw.(pb.NotificationServiceClient)

	reply, err := svc.Notify(context.Background(), &pb.NoticeProto{Level: string(ext.NoticeError), Title: "x"})
	if err != nil {
		t.Fatalf("Notify transport error: %v", err)
	}
	got := pb.ErrorFromProto(reply.Error)
	if got == nil {
		t.Fatal("want a business error, got nil")
	}
	code, ok := ext.CodeOf(got)
	if !ok || code != ext.ErrCodeObjectNotFound {
		t.Fatalf("error code = %v (ok=%v), want ErrCodeObjectNotFound", code, ok)
	}
}
