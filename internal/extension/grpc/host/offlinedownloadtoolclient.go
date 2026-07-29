package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// configRPCTimeout bounds Config's RPC: ext.OfflineDownloadTool.Config takes
// no ctx (the interface's signature is fixed — it is also called in-process
// with no context to thread through), so this client has no caller-supplied
// deadline to forward and would otherwise block forever against a wedged or
// crashed-but-not-yet-reaped subprocess. Config is read frequently on cold
// paths (FindOfflineDownloadToolByName's scan, the settings page's
// OfflineDownloadToolGRPCNames enumeration) but never on a request hot path,
// so a generous fixed timeout is the pragmatic fix rather than plumbing ctx
// through the whole ext.OfflineDownloadTool contract for this one method.
const configRPCTimeout = 10 * time.Second

// OfflineDownloadToolClient is a host-side proxy over a gRPC
// OfflineDownloadTool plugin (Phase E ring 1). It satisfies
// ext.OfflineDownloadTool by forwarding every call over gRPC — mirrors
// NotificationClient (the Notification proxy) one Kind over.
type OfflineDownloadToolClient struct {
	pc  *plugin.Client
	svc pb.OfflineDownloadToolServiceClient

	manifest ext.Manifest
}

var _ ext.OfflineDownloadTool = (*OfflineDownloadToolClient)(nil)

// LaunchOfflineDownloadTool starts the plugin binary described by cmd,
// performs the go-plugin handshake, dispenses the OfflineDownloadToolService
// client, and reads the manifest. The returned client owns the subprocess;
// call Shutdown to stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchNotification (host/notificationclient.go).
func LaunchOfflineDownloadTool(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*OfflineDownloadToolClient, error) {
	plugins := grpcplugin.OfflineDownloadToolPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostOfflineDownloadToolPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: offline download tool plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.OfflineDownloadToolPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.OfflineDownloadToolPluginName, err)
	}
	svc, ok := raw.(pb.OfflineDownloadToolServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an OfflineDownloadToolServiceClient", raw)
	}
	c := &OfflineDownloadToolClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *OfflineDownloadToolClient) loadManifest(ctx context.Context) error {
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
func (c *OfflineDownloadToolClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *OfflineDownloadToolClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *OfflineDownloadToolClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the
// subprocess.
func (c *OfflineDownloadToolClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *OfflineDownloadToolClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.OfflineDownloadTool ---

// Config forwards to the plugin's Config RPC, decoding ToolConfigReply into
// ext.OfflineDownloadToolConfig. Bounded by configRPCTimeout (see its doc
// comment): the ext.OfflineDownloadTool contract gives Config no ctx to
// forward, so this client cannot inherit a caller deadline and must set its
// own rather than risk blocking forever on a wedged subprocess.
func (c *OfflineDownloadToolClient) Config() ext.OfflineDownloadToolConfig {
	ctx, cancel := context.WithTimeout(context.Background(), configRPCTimeout)
	defer cancel()
	reply, err := c.svc.Config(ctx, &pb.Empty{})
	if err != nil {
		return ext.OfflineDownloadToolConfig{}
	}
	return ext.OfflineDownloadToolConfig{Name: reply.Name}
}

// AddURL encodes args as an AddArgsProto and forwards it over the wire —
// mirrors offlineDownloadToolServer.AddURL's decoding on the plugin side.
func (c *OfflineDownloadToolClient) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	reply, err := c.svc.AddURL(ctx, &pb.AddArgsProto{
		Url:     args.URL,
		TempDir: args.TempDir,
		Uid:     args.UID,
	})
	if err != nil {
		return "", err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return "", e
	}
	return reply.Gid, nil
}

// Remove forwards ext.OfflineDownloadTool.Remove over the wire.
func (c *OfflineDownloadToolClient) Remove(ctx context.Context, gid string) error {
	reply, err := c.svc.Remove(ctx, &pb.ToolRemoveRequest{Gid: gid})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}

// Status forwards ext.OfflineDownloadTool.Status over the wire, decoding the
// resulting ToolStatusProto into ext.ToolStatus.
func (c *OfflineDownloadToolClient) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	reply, err := c.svc.Status(ctx, &pb.ToolStatusRequest{Gid: gid})
	if err != nil {
		return ext.ToolStatus{}, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return ext.ToolStatus{}, e
	}
	st := reply.Status
	if st == nil {
		return ext.ToolStatus{}, nil
	}
	return ext.ToolStatus{
		TotalBytes: st.TotalBytes,
		Progress:   st.Progress,
		NewGID:     st.NewGid,
		Completed:  st.Completed,
		Status:     st.Status,
		Err:        st.Err,
	}, nil
}
