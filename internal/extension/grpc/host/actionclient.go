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

// ActionClient is a host-side proxy over a gRPC Action plugin (W8b). It
// satisfies ext.Action by forwarding every call over gRPC — mirrors
// APIRouteClient (the APIRoute proxy) one Kind over.
type ActionClient struct {
	pc  *plugin.Client
	svc pb.ActionServiceClient

	manifest ext.Manifest
}

var _ ext.Action = (*ActionClient)(nil)

// LaunchAction starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the ActionService client, and reads the
// manifest. The returned client owns the subprocess; call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchAPIRoute (host/apirouteclient.go).
func LaunchAction(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*ActionClient, error) {
	plugins := grpcplugin.ActionPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostActionPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: action plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.ActionPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.ActionPluginName, err)
	}
	svc, ok := raw.(pb.ActionServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an ActionServiceClient", raw)
	}
	c := &ActionClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *ActionClient) loadManifest(ctx context.Context) error {
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
func (c *ActionClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *ActionClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *ActionClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *ActionClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *ActionClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.Action ---

// Invoke marshals args as JSON, sends it over the wire, and unmarshals the
// result back into an any — mirrors actionServer.Invoke's encoding on the
// plugin side.
func (c *ActionClient) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	var aj []byte
	if args != nil {
		var err error
		aj, err = json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("host: encode args: %w", err)
		}
	}
	reply, err := c.svc.Invoke(ctx, &pb.InvokeRequest{
		Principal: string(principal),
		ActionId:  actionID,
		ArgsJson:  aj,
	})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	if len(reply.ResultJson) == 0 {
		return nil, nil
	}
	var result any
	if err := json.Unmarshal(reply.ResultJson, &result); err != nil {
		return nil, fmt.Errorf("host: decode result: %w", err)
	}
	return result, nil
}
