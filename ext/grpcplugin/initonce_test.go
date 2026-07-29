package grpcplugin

import (
	"context"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	"github.com/hashicorp/go-plugin"
)

// fakeMultiKindImpl implements both ext.Action and ext.Assist on the same
// value, mirroring actionplugin (internal/extension/grpc/testdata/actionplugin)
// one module over: the SDK's mountAssist auto-mounts this single impl under
// BOTH pb.ActionPluginName and pb.AssistPluginName, which is exactly the
// "same impl, two Kind dispenses sharing one process" shape initOnce exists
// to guard.
type fakeMultiKindImpl struct {
	mu        sync.Mutex
	initCalls int
	lastHost  ext.Host
}

func (d *fakeMultiKindImpl) Manifest() ext.Manifest             { return ext.Manifest{ID: "fake-multi-kind"} }
func (d *fakeMultiKindImpl) Shutdown(ctx context.Context) error { return nil }

func (d *fakeMultiKindImpl) Init(ctx context.Context, host ext.Host) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initCalls++
	d.lastHost = host
	return nil
}

func (d *fakeMultiKindImpl) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	return nil, nil
}

func (d *fakeMultiKindImpl) Assist(ctx context.Context, principal ext.Principal, formID string, input map[string]any) (any, error) {
	return nil, nil
}

var _ ext.Action = (*fakeMultiKindImpl)(nil)
var _ ext.Assist = (*fakeMultiKindImpl)(nil)

func (d *fakeMultiKindImpl) snapshot() (int, ext.Host) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.initCalls, d.lastHost
}

// TestInitOnce_SecondKindDispenseReusesFirstInit proves the core PR-2
// guarantee end to end using the real go-plugin handshake machinery
// (plugin.TestPluginGRPCConn, same harness as inithost_test.go): when one
// impl is dispensed under two different Kind names in the same process
// (here: Action, then its auto-mounted Assist leg — mountAssist), only the
// FIRST dispense's InitHost actually calls impl.Init. The second reuses that
// outcome and never even dials a second host connection.
func TestInitOnce_SecondKindDispenseReusesFirstInit(t *testing.T) {
	impl := &fakeMultiKindImpl{}
	plugins := map[string]plugin.Plugin{
		pb.ActionPluginName: &ActionPlugin{Impl: impl, HostServerFactory: hostServerFactory},
		pb.AssistPluginName: &AssistPlugin{Impl: impl, HostServerFactory: hostServerFactory},
	}

	client, server := plugin.TestPluginGRPCConn(t, false, plugins)
	defer client.Close()
	defer server.Stop()

	if _, err := client.Dispense(pb.ActionPluginName); err != nil {
		t.Fatalf("dispense action: %v", err)
	}
	calls, host := impl.snapshot()
	if calls != 1 {
		t.Fatalf("initCalls after first (action) dispense = %d, want 1", calls)
	}
	if host == nil {
		t.Fatal("Init must have received a non-nil host after the first dispense")
	}

	if _, err := client.Dispense(pb.AssistPluginName); err != nil {
		t.Fatalf("dispense assist: %v", err)
	}
	calls, host2 := impl.snapshot()
	if calls != 1 {
		t.Fatalf("initCalls after second (assist) dispense = %d, want still 1 (initOnce must not call Init twice)", calls)
	}
	if host2 == nil {
		t.Fatal("host must still be non-nil after the second dispense reused the first outcome")
	}

	// The host handed to Init the one time it ran must actually be live and
	// working — round-trip a call through it, mirroring
	// TestInitHost_WithHostServerFactory.
	cfg, err := host2.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("host.LoadConfig: %v", err)
	}
	if string(cfg) != `{"hello":"host"}` {
		t.Fatalf("host.LoadConfig = %s, want the fakeHostServer payload", cfg)
	}
}

// stubHost is a minimal, non-networked ext.Host used only to unit-test
// initOnce's own locking/memoization in isolation, without standing up a
// real broker/gRPC connection (that end-to-end path is covered by
// TestInitOnce_SecondKindDispenseReusesFirstInit above).
type stubHost struct{}

func (stubHost) Logger() ext.Logger                                 { return nil }
func (stubHost) FS() ext.FS                                         { return nil }
func (stubHost) Auth() ext.Auth                                     { return nil }
func (stubHost) Store() ext.Store                                   { return nil }
func (stubHost) Sign() ext.Signer                                   { return nil }
func (stubHost) HTTP() ext.HTTPClient                               { return nil }
func (stubHost) Cache() ext.Cache                                   { return nil }
func (stubHost) LoadConfig(ctx context.Context) ([]byte, error)     { return nil, nil }
func (stubHost) SaveConfig(ctx context.Context, cfg []byte) error   { return nil }
func (stubHost) Capabilities(ctx context.Context) ([]string, error) { return nil, nil }

var _ ext.Host = stubHost{}

// TestInitOnce_ConcurrentCallsRunInitExactlyOnce pins the race-safety half of
// the guarantee: many goroutines calling initOnce for the SAME impl at once
// (simulating several Kind dispenses racing to InitHost concurrently) must
// still result in exactly one impl.Init call, and every caller must observe
// that single call's result. Run with -race to catch any unsynchronized
// access to the shared initState.
func TestInitOnce_ConcurrentCallsRunInitExactlyOnce(t *testing.T) {
	impl := &fakeMultiKindImpl{}
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = initOnce(context.Background(), impl, func() (ext.Host, error) {
				return stubHost{}, nil
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("initOnce[%d] = %v, want nil", i, err)
		}
	}
	calls, host := impl.snapshot()
	if calls != 1 {
		t.Fatalf("initCalls = %d, want 1 (concurrent InitHost calls must collapse into a single Init)", calls)
	}
	if host == nil {
		t.Fatal("Init must have received a non-nil host")
	}
}
