// Command actionplugin serves a synthetic Kind=Action extension as a
// go-plugin gRPC plugin (the menu-action-execution mechanism-proving test
// double — v4 ships no real Action plugin yet). It echoes the invocation
// (principal/actionID/args) back as its result, so a host test can prove the
// full InvokeExtensionAction round trip against a real subprocess. Mirrors
// apirouteplugin (testdata/apirouteplugin) one Kind over.
//
// It also implements ext.Assist (assist gRPC leg, Task 3): the same impl
// declares one assist form ("probe") and echoes the submission back, so the
// same binary that already proves the Action leg also proves ServeAction's
// auto-mount of the "assist" dispense name against a real subprocess.
//
// G2 addition: it also implements ext.EventHook (OnEvent recording every
// event it sees), so this one binary is a real two-primary-Kind (Action +
// EventHook) + assist plugin — the shape host.LaunchShared/SharedClient and
// pluginproc's shared-process pool need a genuine multi-Kind fixture to
// prove against, without standing up a brand new testdata binary. main()
// keeps calling grpcplugin.ServeAction (the pre-G2, single-primary-Kind
// entry point) unchanged, so every existing test exercising this fixture's
// Action/Assist behavior is unaffected; G2's own tests launch it via
// grpcplugin.ServeMulti instead (see sharedserve_test.go/multikindplugin, or
// a dedicated shared-mode fixture) to exercise the Action+EventHook combo
// out of one process.
package main

import (
	"context"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type actionPlugin struct {
	mu     sync.Mutex
	events []ext.Event
}

var _ ext.Action = (*actionPlugin)(nil)
var _ ext.Assist = (*actionPlugin)(nil)
var _ ext.EventHook = (*actionPlugin)(nil)

func (p *actionPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testaction",
		Name:    map[string]string{"en": "Test Action"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindAction, ext.KindEventHook},
		Contributions: ext.Contributions{
			MenuActions: []ext.MenuAction{
				{ID: "echo", Label: map[string]string{"en": "Echo"}, Target: "global"},
			},
			AssistForms: []ext.AssistForm{
				{ID: "probe", Title: map[string]string{"en": "Probe"}},
			},
		},
	}
}

func (p *actionPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *actionPlugin) Shutdown(ctx context.Context) error         { return nil }

// Invoke echoes the invocation back, EXCEPT for the reserved actionID
// "__events": that one instead reports how many OnEvent calls this process
// instance has observed and their types, so a test can prove EventHook and
// Action were actually served out of the SAME running instance (not just
// that neither RPC errored) — e.g. dispensing both off one host.SharedClient
// and confirming an OnEvent call is visible to a subsequent "__events"
// Invoke call.
func (p *actionPlugin) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	if actionID == "__events" {
		p.mu.Lock()
		defer p.mu.Unlock()
		types := make([]string, len(p.events))
		for i, e := range p.events {
			types[i] = string(e.Type)
		}
		return map[string]any{"count": len(p.events), "types": types}, nil
	}
	return map[string]any{
		"principal": string(principal),
		"action_id": actionID,
		"args":      args,
	}, nil
}

func (p *actionPlugin) Assist(ctx context.Context, principal ext.Principal, formID string, input map[string]any) (any, error) {
	return map[string]any{
		"form": formID,
		"echo": input,
	}, nil
}

// OnEvent records the event so a test can assert on how many/which events
// this process observed, exposed through the "__events" Invoke escape hatch
// above (no new gRPC surface needed).
func (p *actionPlugin) OnEvent(ctx context.Context, e ext.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

// main serves via ServeMulti (not the single-Kind ServeAction) so this one
// process actually registers ActionService, EventHookService, AND the
// auto-mounted AssistService together — the real multi-Kind, single-process
// dispense shape G2's host.LaunchShared/SharedClient and pluginproc's shared
// pool are built to exercise. mountAll (ServeMulti's map builder) produces a
// strict SUPERSET of what the old ServeAction/ActionPluginMap(p) alone
// mounted (same "action"+"assist" keys, plus "event_hook"), so every
// existing test that only ever dispenses "action" or "assist" against this
// binary (host.LaunchAction, host.LaunchAssist, and everything built on
// them) is unaffected — it simply never asks for the new key.
func main() {
	grpcplugin.ServeMulti(&actionPlugin{})
}
