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

// EventHookClient is a host-side proxy over a gRPC EventHook plugin (W7). It
// satisfies ext.EventHook by forwarding every call over gRPC — mirrors
// ActionClient (the Action proxy) one Kind over.
type EventHookClient struct {
	pc  *plugin.Client
	svc pb.EventHookServiceClient

	manifest ext.Manifest
}

var _ ext.EventHook = (*EventHookClient)(nil)

// LaunchEventHook starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the EventHookService client, and reads the
// manifest. The returned client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchAction (host/actionclient.go).
func LaunchEventHook(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*EventHookClient, error) {
	plugins := grpcplugin.EventHookPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostEventHookPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: eventhook plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.EventHookPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.EventHookPluginName, err)
	}
	svc, ok := raw.(pb.EventHookServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an EventHookServiceClient", raw)
	}
	c := &EventHookClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *EventHookClient) loadManifest(ctx context.Context) error {
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
func (c *EventHookClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *EventHookClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *EventHookClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *EventHookClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *EventHookClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.EventHook ---

// OnEvent encodes ev as an EventProto and forwards it over the wire —
// mirrors eventHookServer.OnEvent's decoding on the plugin side.
func (c *EventHookClient) OnEvent(ctx context.Context, e ext.Event) error {
	reply, err := c.svc.OnEvent(ctx, &pb.EventProto{
		Type:      string(e.Type),
		UnixMilli: e.UnixMilli,
		Path:      e.Path,
		DstPath:   e.DstPath,
		Actor:     e.Actor,
	})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}
