package fs

import (
	"context"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
)

// capturingHook records the events it receives.
type capturingHook struct {
	mu  sync.Mutex
	got []ext.Event
}

func (h *capturingHook) Manifest() ext.Manifest               { return ext.Manifest{} }
func (h *capturingHook) Init(context.Context, ext.Host) error { return nil }
func (h *capturingHook) Shutdown(context.Context) error       { return nil }
func (h *capturingHook) OnEvent(_ context.Context, e ext.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, e)
	return nil
}

func (h *capturingHook) events() []ext.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ext.Event(nil), h.got...)
}

// TestFSMakeDirAndRemoveFireEventHook drives the real fs.MakeDir/fs.Remove path
// and asserts a granted EventHook receives an fs.mkdir then fs.remove event,
// carrying the acting user as the actor.
func TestFSMakeDirAndRemoveFireEventHook(t *testing.T) {
	setupMoveTestStorage(t, "/evt")

	hook := &capturingHook{}
	// Register an enabled, in-process Kind=EventHook plugin entry declaring the
	// "events" grant (RegisterPluginMode is the exported constructor; its manifest
	// permissions drive the dispatch grant gate).
	man := ext.Manifest{
		ID:          "evt.hook",
		Kinds:       []ext.Kind{ext.KindEventHook},
		Permissions: ext.Permissions{HostAPIs: []string{extension.CapEvents}},
	}
	if err := extension.DefaultRegistry.RegisterPluginMode(man, "", extension.ExecInProcess); err != nil {
		t.Fatal(err)
	}
	extension.RegisterEventHookInstance("evt.hook", hook)
	t.Cleanup(func() {
		extension.RegisterEventHookInstance("evt.hook", nil)
		extension.DefaultRegistry.Deregister("evt.hook")
	})

	ctx := context.WithValue(context.Background(), ctxkey.User, &model.User{ID: 1, Username: "alice"})
	if err := MakeDir(ctx, "/evt/sub"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := Remove(ctx, "/evt/sub"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	evs := hook.events()
	if len(evs) != 2 {
		t.Fatalf("want 2 events (mkdir, remove), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != ext.EventFSMkdir || evs[0].Path != "/evt/sub" || evs[0].Actor != "alice" {
		t.Errorf("mkdir event wrong: %+v", evs[0])
	}
	if evs[1].Type != ext.EventFSRemove || evs[1].Path != "/evt/sub" {
		t.Errorf("remove event wrong: %+v", evs[1])
	}
}
