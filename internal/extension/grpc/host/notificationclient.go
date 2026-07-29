package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// NotificationClient is a host-side proxy over a gRPC Notification plugin
// (W7b). It satisfies ext.Notification by forwarding every call over gRPC —
// mirrors EventHookClient (the EventHook proxy) one Kind over.
type NotificationClient struct {
	pc  *plugin.Client
	svc pb.NotificationServiceClient

	manifest ext.Manifest
}

var _ ext.Notification = (*NotificationClient)(nil)

// LaunchNotification starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the NotificationService client, and reads
// the manifest. The returned client owns the subprocess; call Shutdown to
// stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchEventHook (host/eventhookclient.go).
func LaunchNotification(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*NotificationClient, error) {
	plugins := grpcplugin.NotificationPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostNotificationPluginMap(NewHostServiceServer(hostBuilder()))
	}
	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  grpcplugin.Handshake,
		Plugins:          plugins,
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           hclog.NewNullLogger(),
	})
	rpcClient, err := pc.Client()
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: notification plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.NotificationPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.NotificationPluginName, err)
	}
	svc, ok := raw.(pb.NotificationServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not a NotificationServiceClient", raw)
	}
	c := &NotificationClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *NotificationClient) loadManifest(ctx context.Context) error {
	mr, err := c.svc.GetManifest(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("host: GetManifest: %w", err)
	}
	if err := json.Unmarshal(mr.ManifestJson, &c.manifest); err != nil {
		return fmt.Errorf("host: decode manifest: %w", err)
	}
	return nil
}

// Exited reports whether the plugin subprocess has already died. See
// Client.Exited (host/client.go) for the rationale — same go-plugin reaper.
func (c *NotificationClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *NotificationClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *NotificationClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the
// subprocess.
func (c *NotificationClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *NotificationClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.Notification ---

// Notify encodes n as a NoticeProto and forwards it over the wire — mirrors
// notificationServer.Notify's decoding on the plugin side.
func (c *NotificationClient) Notify(ctx context.Context, n ext.Notice) error {
	reply, err := c.svc.Notify(ctx, &pb.NoticeProto{
		Level:     string(n.Level),
		Title:     n.Title,
		Body:      n.Body,
		Source:    n.Source,
		UnixMilli: n.UnixMilli,
	})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}
