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

// ScheduledJobClient is a host-side proxy over a gRPC ScheduledJob plugin
// (W7). It satisfies ext.ScheduledJob by forwarding every call over gRPC —
// mirrors EventHookClient (the EventHook proxy) one Kind over.
type ScheduledJobClient struct {
	pc  *plugin.Client
	svc pb.ScheduledJobServiceClient

	manifest ext.Manifest
}

var _ ext.ScheduledJob = (*ScheduledJobClient)(nil)

// LaunchScheduledJob starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the ScheduledJobService client, and reads
// the manifest. The returned client owns the subprocess; call Shutdown to
// stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchEventHook (host/eventhookclient.go).
func LaunchScheduledJob(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*ScheduledJobClient, error) {
	plugins := grpcplugin.ScheduledJobPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostScheduledJobPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: scheduledjob plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.ScheduledJobPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.ScheduledJobPluginName, err)
	}
	svc, ok := raw.(pb.ScheduledJobServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not a ScheduledJobServiceClient", raw)
	}
	c := &ScheduledJobClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *ScheduledJobClient) loadManifest(ctx context.Context) error {
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
func (c *ScheduledJobClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *ScheduledJobClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *ScheduledJobClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *ScheduledJobClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *ScheduledJobClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.ScheduledJob ---

// Schedule forwards ext.ScheduledJob.Schedule's cadence spec over the wire.
// On a transport error it returns the empty spec, which ParseSchedule rejects
// as unparseable — the caller's disable-on-error path (see
// internal/extension/scheduler.go's Start) then simply skips the job rather
// than propagating a gRPC error through a method that returns no error.
func (c *ScheduledJobClient) Schedule() string {
	reply, err := c.svc.Schedule(context.Background(), &pb.Empty{})
	if err != nil {
		return ""
	}
	return reply.Spec
}

func (c *ScheduledJobClient) Run(ctx context.Context) error {
	reply, err := c.svc.Run(ctx, &pb.Empty{})
	if err != nil {
		return err
	}
	return pb.ErrorFromProto(reply.Error)
}
