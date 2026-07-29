// Command lyingkindplugin serves a synthetic Kind=Action extension whose
// manifest LIES about also implementing Kind=EventHook: Manifest.Kinds lists
// both, but the underlying Go value only ever implements ext.Action, never
// ext.EventHook. This is a "declares a Kind it doesn't actually serve" test
// double for host.SharedClient.dispenseKind's manifest-says-yes-but-peer-
// never-registered-the-service path (G2 review round 1, item 2) — the
// realistic shape a buggy or stale plugin build takes: Manifest.Kinds
// updated ahead of (or independently from) the Kind actually being wired up.
package main

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type lyingKindPlugin struct{}

var _ ext.Action = (*lyingKindPlugin)(nil)

func (p *lyingKindPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.lyingkind",
		Name:    map[string]string{"en": "Lying Kind Test"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		// Declares EventHook here even though this value never implements
		// ext.EventHook below — deliberately, to simulate a manifest that
		// lies about (or is stale relative to) what the binary actually
		// serves.
		Kinds: []ext.Kind{ext.KindAction, ext.KindEventHook},
	}
}

func (p *lyingKindPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *lyingKindPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *lyingKindPlugin) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	return map[string]any{"action_id": actionID}, nil
}

// main deliberately serves via ServeMulti with an impl that only really
// implements ext.Action — mountAll only mounts what it can type-assert
// against, so EventHookService never gets registered on the shared
// grpc.Server despite the manifest above claiming Kind=EventHook too.
func main() {
	grpcplugin.ServeMulti(&lyingKindPlugin{})
}
