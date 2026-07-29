// Command eventhookplugin serves a synthetic Kind=EventHook extension as a
// go-plugin gRPC plugin (the fs-event notification-sink mechanism-proving
// test double — v4 ships no real EventHook plugin yet). ext.EventHook.OnEvent
// returns only an error (no positive result channel), so this fixture proves
// delivery-intact over the wire by echoing the received Event's fields back
// as the OnEvent error message — a host test decodes that message to assert
// every field crossed the gRPC boundary unmangled. Mirrors apirouteplugin
// (testdata/apirouteplugin) one Kind over.
package main

import (
	"context"
	"fmt"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type eventHookPlugin struct{}

var _ ext.EventHook = (*eventHookPlugin)(nil)

func (p *eventHookPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testeventhook",
		Name:    map[string]string{"en": "Test Event Hook"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindEventHook},
	}
}

func (p *eventHookPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *eventHookPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *eventHookPlugin) OnEvent(ctx context.Context, e ext.Event) error {
	return fmt.Errorf("echo:%s:%d:%s:%s:%s", e.Type, e.UnixMilli, e.Path, e.DstPath, e.Actor)
}

func main() {
	grpcplugin.ServeEventHook(&eventHookPlugin{})
}
