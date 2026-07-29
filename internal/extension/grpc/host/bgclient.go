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

// BgClient is a host-side proxy over a gRPC Kind=BackgroundService plugin: a
// long-lived task (FRP tunnel, sync engine) that runs inside its own
// subprocess. It satisfies ext.BackgroundService by forwarding lifecycle over
// gRPC, so the ServiceSupervisor can treat a subprocess service exactly like an
// in-process one — mirrors AuthClient one Kind over.
//
// The wire model is Start/Stop/Status unary RPCs, but ext.BackgroundService.Run
// is blocking; Run bridges them (Start, then block on ctx, then Stop) so the
// supervisor's "call Run, it blocks until cancel" contract holds unchanged.
type BgClient struct {
	pc  *plugin.Client
	svc pb.BackgroundServiceServiceClient

	manifest ext.Manifest
}

var _ ext.BackgroundService = (*BgClient)(nil)

// bgStopTimeout bounds the graceful-stop RPC issued when Run's context is
// cancelled: the subprocess must be told to halt its task, but a wedged plugin
// must not hang shutdown (the supervisor's own join grace is the backstop).
const bgStopTimeout = 5 * time.Second

// LaunchBackgroundService starts the plugin binary described by cmd, performs
// the go-plugin handshake, dispenses the BackgroundServiceService client, and
// reads the manifest. The returned client owns the subprocess; call Shutdown to
// stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors Launch (host/client.go).
func LaunchBackgroundService(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*BgClient, error) {
	plugins := grpcplugin.BackgroundServicePluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostBackgroundServicePluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: background service plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.BgServicePluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.BgServicePluginName, err)
	}
	svc, ok := raw.(pb.BackgroundServiceServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not a BackgroundServiceServiceClient", raw)
	}
	c := &BgClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *BgClient) loadManifest(ctx context.Context) error {
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
func (c *BgClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process hasn't
// started (or already exited). Diagnostic use only (e.g. tests simulating a
// crash via kill -9) — mirrors Client.Pid.
func (c *BgClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *BgClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *BgClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *BgClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.BackgroundService ---

// Run tells the subprocess to Start its task, blocks until ctx is cancelled
// (host shutdown or a StopService), then issues a bounded graceful Stop. It
// mirrors an in-process service's blocking Run so the supervisor's lifecycle is
// identical whether the service is in-process or out-of-process.
func (c *BgClient) Run(ctx context.Context) error {
	if _, err := c.svc.Start(ctx, &pb.Empty{}); err != nil {
		return err
	}
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), bgStopTimeout)
	defer cancel()
	_, _ = c.svc.Stop(stopCtx, &pb.Empty{})
	return nil
}

// Status forwards to the subprocess and decodes the reported state/metrics.
func (c *BgClient) Status(ctx context.Context) (ext.ServiceStatus, error) {
	r, err := c.svc.Status(ctx, &pb.Empty{})
	if err != nil {
		return ext.ServiceStatus{}, err
	}
	if e := pb.ErrorFromProto(r.GetError()); e != nil {
		return ext.ServiceStatus{}, e
	}
	st := ext.ServiceStatus{State: r.GetState(), Detail: r.GetDetail()}
	if raw := r.GetMetricsJson(); len(raw) > 0 {
		var metrics map[string]any
		if err := json.Unmarshal(raw, &metrics); err == nil {
			st.Metrics = metrics
		}
	}
	return st, nil
}
