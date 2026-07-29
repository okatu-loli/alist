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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AssistClient is a host-side proxy over a gRPC assist auxiliary service
// (assist gRPC leg, Task 2). It satisfies ext.Assist by forwarding every call
// over gRPC — mirrors ActionClient (actionclient.go) one service over, minus
// the manifest RPC (AssistService deliberately has none — see assist.proto:
// the manifest handshake already happened on the plugin's primary Kind
// service).
type AssistClient struct {
	pc  *plugin.Client
	svc pb.AssistServiceClient
}

var _ ext.Assist = (*AssistClient)(nil)

// LaunchAssist starts the plugin binary described by cmd, performs the
// go-plugin handshake, and dispenses the AssistService client.
//
// Per the assist gRPC leg design (independent assist process pool): this
// always launches a DEDICATED subprocess that only ever dispenses "assist" —
// it is never shared with a StorageDriver/Action/... subprocess already
// running for the same plugin ID (pluginproc.Manager keeps a separate
// assistProcs pool for exactly this reason). That keeps impl.Init to exactly
// one call: the reverse-host handshake below (when hostBuilder is set) runs
// inside this fresh process via InitHost, so there is no risk of a second,
// redundant Init against an impl some other dispense already initialized.
//
// hostBuilder, when non-nil, is called once to build the ext.Host this
// launch should hand the plugin via the reverse InitHost bridge (F0/SP6). nil
// (the default) keeps pre-bridge behavior — no reverse host is served and the
// plugin's Init is never called.
//
// A plugin binary whose impl does not implement ext.Assist never mounts the
// "assist" dispense name (mountAssist, ext/grpcplugin/assistserve.go), so
// Dispense fails here; that failure is reported back to the caller as
// ext.ErrCodeNotImplement — the old/plain-plugin compatibility path — rather
// than a plain transport error, so a caller resolving assist for such a
// plugin gets the same NOT_IMPLEMENT seam as an entry with no gRPC factory at
// all.
func LaunchAssist(ctx context.Context, cmd *exec.Cmd, hostBuilder func() ext.Host) (*AssistClient, error) {
	plugins := grpcplugin.AssistPluginMap(nil)
	if hostBuilder != nil {
		plugins = grpcplugin.HostAssistPluginMap(NewHostServiceServer(hostBuilder()))
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
		return nil, fmt.Errorf("host: assist plugin handshake failed: %w", err)
	}
	raw, err := rpcClient.Dispense(pb.AssistPluginName)
	if err != nil {
		pc.Kill()
		return nil, ext.NewError(ext.ErrCodeNotImplement,
			fmt.Sprintf("host: plugin does not implement assist: %v", err))
	}
	svc, ok := raw.(pb.AssistServiceClient)
	if !ok {
		pc.Kill()
		return nil, fmt.Errorf("host: dispensed %T is not an AssistServiceClient", raw)
	}
	return &AssistClient{pc: pc, svc: svc}, nil
}

// Exited reports whether the plugin subprocess has already died. See
// Client.Exited (client.go) for the rationale — same go-plugin reaper.
func (c *AssistClient) Exited() bool { return c.pc.Exited() }

// Pid returns the plugin subprocess's OS process ID, or 0 if the process
// hasn't started (or already exited and released its handle). Diagnostic use
// only. Mirrors Client.Pid (client.go).
func (c *AssistClient) Pid() int {
	rc := c.pc.ReattachConfig()
	if rc == nil {
		return 0
	}
	return rc.Pid
}

// Shutdown kills the plugin subprocess.
func (c *AssistClient) Shutdown(ctx context.Context) error {
	c.pc.Kill()
	return nil
}

// --- ext.Assist ---

// Assist marshals input as JSON, sends it over the wire, and unmarshals the
// result back into an any — mirrors assistServer.Assist's encoding on the
// plugin side (and ActionClient.Invoke one service over).
//
// A gRPC Unimplemented status from the peer — the other shape an
// old/partial plugin implementation of AssistService can take, distinct from
// the Dispense-time failure LaunchAssist already maps — is likewise mapped
// to ext.ErrCodeNotImplement rather than surfaced as a raw transport error.
func (c *AssistClient) Assist(ctx context.Context, principal ext.Principal, formID string, input map[string]any) (any, error) {
	var ij []byte
	if input != nil {
		var err error
		ij, err = json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("host: encode input: %w", err)
		}
	}
	reply, err := c.svc.Assist(ctx, &pb.AssistRequest{
		Principal: string(principal),
		FormId:    formID,
		InputJson: ij,
	})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return nil, ext.NewError(ext.ErrCodeNotImplement,
				fmt.Sprintf("host: assist form %q not implemented by plugin: %v", formID, err))
		}
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
