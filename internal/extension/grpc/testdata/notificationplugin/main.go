// Command notificationplugin serves a synthetic Kind=NotificationChannel
// extension as a go-plugin gRPC plugin (the notification-sink
// mechanism-proving test double — v4 ships no real Notification plugin yet).
// ext.Notification.Notify returns only an error (no positive result
// channel), so this fixture proves delivery-intact over the wire by echoing
// the received Notice's fields back as the Notify error message — a host
// test decodes that message to assert every field crossed the gRPC boundary
// unmangled. Mirrors eventhookplugin (testdata/eventhookplugin) one Kind
// over.
package main

import (
	"context"
	"fmt"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type notificationPlugin struct{}

var _ ext.Notification = (*notificationPlugin)(nil)

func (p *notificationPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testnotification",
		Name:    map[string]string{"en": "Test Notification Channel"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindNotification},
	}
}

func (p *notificationPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *notificationPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *notificationPlugin) Notify(ctx context.Context, n ext.Notice) error {
	return fmt.Errorf("echo:%s:%s:%s:%s:%d", n.Level, n.Title, n.Body, n.Source, n.UnixMilli)
}

func main() {
	grpcplugin.ServeNotification(&notificationPlugin{})
}
