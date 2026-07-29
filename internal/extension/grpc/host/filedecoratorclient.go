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

// FileDecoratorClient is a host-side proxy over a gRPC FileDecorator plugin
// (W9a). It satisfies ext.FileDecorator by forwarding every call over gRPC —
// mirrors EventHookClient (the EventHook proxy) one Kind over.
type FileDecoratorClient struct {
	pc  *plugin.Client
	svc pb.FileDecoratorServiceClient

	manifest ext.Manifest
	// groups is derived once from manifest.Contributions.FileColumns (the
	// unique FieldGroup values, in first-appearance order) — there is no
	// separate Groups RPC (see filedecorator.proto's doc comment).
	groups []string
}

var _ ext.FileDecorator = (*FileDecoratorClient)(nil)

// LaunchFileDecorator starts the plugin binary described by cmd, performs the
// go-plugin handshake, dispenses the FileDecoratorService client, and reads
// the manifest. The returned client owns the subprocess; call Shutdown to
// stop it.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this launch
// should hand the plugin via the reverse InitHost bridge (F0/SP6): the host
// side serves a HostService backed by hostBuilder() on a broker id, and the
// plugin dials it and calls impl.Init with a working host. nil (the default)
// keeps pre-bridge behavior — no reverse host is served and the plugin's Init
// is never called. Mirrors LaunchEventHook (host/eventhookclient.go).
func LaunchFileDecorator(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*FileDecoratorClient, error) {
	plugins := grpcplugin.FileDecoratorPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostFileDecoratorPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: filedecorator plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.FileDecoratorPluginName)
	if err != nil {
		pc.Kill()
		return nil, fmt.Errorf("host: dispense %q: %w", pb.FileDecoratorPluginName, err)
	}
	svc, ok := raw.(pb.FileDecoratorServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not a FileDecoratorServiceClient", raw)
	}
	c := &FileDecoratorClient{pc: pc, svc: svc}
	if err := c.loadManifest(ctx); err != nil {
		pc.Kill()
		return nil, err
	}
	return c, nil
}

func (c *FileDecoratorClient) loadManifest(ctx context.Context) error {
	mr, err := c.svc.GetManifest(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("host: GetManifest: %w", err)
	}
	if err := json.Unmarshal(mr.ManifestJson, &c.manifest); err != nil {
		return fmt.Errorf("host: decode manifest: %w", err)
	}
	c.groups = groupsFromContributions(c.manifest)
	return nil
}

// groupsFromContributions collects the unique FieldGroup values declared in
// manifest.Contributions.FileColumns, in first-appearance order — the
// FileDecorator.Groups() contract this decorator implements (see
// ext.FileDecorator.Groups' doc comment: "matching a FileColumn
// contribution's FieldGroup").
func groupsFromContributions(m ext.Manifest) []string {
	var out []string
	seen := map[string]bool{}
	for _, col := range m.Contributions.FileColumns {
		if col.FieldGroup == "" || seen[col.FieldGroup] {
			continue
		}
		seen[col.FieldGroup] = true
		out = append(out, col.FieldGroup)
	}
	return out
}

// Exited reports whether the plugin subprocess has already died. See
// Client.Exited (host/client.go) for the rationale — same go-plugin reaper.
func (c *FileDecoratorClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only (e.g. tests simulating a crash via kill -9). Mirrors Client.Pid
// (host/client.go).
func (c *FileDecoratorClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// --- Extension (process-level) ---

func (c *FileDecoratorClient) Manifest() ext.Manifest { return c.manifest }

// Init is a no-op: the plugin's process-level Init runs inside the subprocess.
func (c *FileDecoratorClient) Init(ctx context.Context, h ext.Host) error { return nil }

// Shutdown kills the plugin subprocess.
func (c *FileDecoratorClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.FileDecorator ---

// Groups returns the field groups this decorator provides, derived from the
// manifest loaded at launch (see groupsFromContributions).
func (c *FileDecoratorClient) Groups() []string { return c.groups }

// Decorate encodes group+refs and forwards them over the wire, decoding the
// plugin's values_json back into a map[string]any — mirrors
// fileDecoratorServer.Decorate's encoding on the plugin side.
func (c *FileDecoratorClient) Decorate(ctx context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	protoRefs := make([]*pb.ObjRef, len(refs))
	for i, r := range refs {
		protoRefs[i] = pb.RefToProto(r)
	}
	reply, err := c.svc.Decorate(ctx, &pb.DecorateRequest{Group: group, Refs: protoRefs})
	if err != nil {
		return nil, err
	}
	if e := pb.ErrorFromProto(reply.Error); e != nil {
		return nil, e
	}
	if len(reply.ValuesJson) == 0 {
		return nil, nil
	}
	var values map[string]any
	if err := json.Unmarshal(reply.ValuesJson, &values); err != nil {
		return nil, fmt.Errorf("host: decode values: %w", err)
	}
	return values, nil
}
