package ext

import (
	"context"
	"testing"
)

// stubAction adapts to the Action interface, echoing the actionID + args.
type stubAction struct{}

func (stubAction) Manifest() Manifest               { return Manifest{ID: "stub.action"} }
func (stubAction) Init(context.Context, Host) error { return nil }
func (stubAction) Shutdown(context.Context) error   { return nil }
func (stubAction) Invoke(_ context.Context, p Principal, actionID string, args map[string]any) (any, error) {
	return map[string]any{"principal": string(p), "action": actionID, "args": args}, nil
}

func TestActionInterfaceShape(t *testing.T) {
	var a Action = stubAction{}
	got, err := a.Invoke(context.Background(), Principal("h"), "share.create", map[string]any{"path": "/x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["action"] != "share.create" || m["principal"] != "h" {
		t.Fatalf("unexpected result: %+v", got)
	}
}
