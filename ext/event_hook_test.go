package ext

import (
	"context"
	"testing"
)

// stubEventHook is a minimal in-process EventHook for exercising the interface
// shape and the Extension embedding.
type stubEventHook struct {
	got []Event
}

func (s *stubEventHook) Manifest() Manifest               { return Manifest{ID: "stub.eventhook"} }
func (s *stubEventHook) Init(context.Context, Host) error { return nil }
func (s *stubEventHook) Shutdown(context.Context) error   { return nil }
func (s *stubEventHook) OnEvent(_ context.Context, e Event) error {
	s.got = append(s.got, e)
	return nil
}

func TestEventHookInterfaceShape(t *testing.T) {
	var h EventHook = &stubEventHook{}
	ev := Event{Type: EventFSMove, Path: "/a/x.txt", DstPath: "/b", Actor: "alice"}
	if err := h.OnEvent(context.Background(), ev); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	s := h.(*stubEventHook)
	if len(s.got) != 1 || s.got[0].Type != EventFSMove || s.got[0].DstPath != "/b" {
		t.Fatalf("event not delivered as-sent: %+v", s.got)
	}
}
