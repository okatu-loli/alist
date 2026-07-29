// Command bgplugin serves a synthetic Kind=BackgroundService extension as a
// go-plugin gRPC plugin (the FRP-external-plugin mechanism-proving test double —
// it needs no real frps to exercise the Start/Status/Stop lifecycle over the
// wire). It exercises the full BackgroundService round trip: Run blocks until
// its context is cancelled (recording running/stopped transitions), and Status
// reports the three lifecycle states plus metrics.
//
// Behavior is selected by environment variables (inherited from the parent,
// since host.LaunchBackgroundService leaves exec.Cmd.Env nil):
//
//   - BGPLUGIN_FORCE_ERROR=1: Status always reports the "error" state with an
//     *ext.Error, so the host side can prove an error status round-trips through
//     ErrorProto.
package main

import (
	"context"
	"os"
	"sync"
	"sync/atomic"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

const envForceError = "BGPLUGIN_FORCE_ERROR"

type bgPlugin struct {
	forceError bool

	mu      sync.Mutex
	running bool
	// starts counts how many times Run has been entered, exposed as a metric so
	// the host side can assert Start actually reached the impl.
	starts int64
}

var _ ext.BackgroundService = (*bgPlugin)(nil)

func (p *bgPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testbg",
		Name:    map[string]string{"en": "Test Background Service"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindBackgroundService},
	}
}

func (p *bgPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *bgPlugin) Shutdown(ctx context.Context) error         { return nil }

// Run blocks until ctx is cancelled (Stop), flipping running true/false around
// the block so Status observes the transition.
func (p *bgPlugin) Run(ctx context.Context) error {
	atomic.AddInt64(&p.starts, 1)
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()

	<-ctx.Done()

	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	return nil
}

func (p *bgPlugin) Status(ctx context.Context) (ext.ServiceStatus, error) {
	if p.forceError {
		return ext.ServiceStatus{}, ext.NewError(ext.ErrCodeNotSupport, "bgplugin: forced error state")
	}
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	state := ext.ServiceStateStopped
	if running {
		state = ext.ServiceStateRunning
	}
	return ext.ServiceStatus{
		State:   state,
		Detail:  "synthetic tunnel",
		Metrics: map[string]any{"starts": atomic.LoadInt64(&p.starts)},
	}, nil
}

func main() {
	grpcplugin.ServeBackgroundService(&bgPlugin{
		forceError: os.Getenv(envForceError) == "1",
	})
}
