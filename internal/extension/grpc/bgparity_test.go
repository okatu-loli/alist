package grpc

import (
	"context"
	"os/exec"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchBgPlugin starts the bgplugin fixture over a real go-plugin subprocess.
// host.LaunchBackgroundService leaves exec.Cmd.Env nil, so the child inherits
// the parent's environment — any t.Setenv here is visible to the subprocess.
func launchBgPlugin(t *testing.T) *host.BgClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.LaunchBackgroundService(context.Background(), exec.Command(bgPluginBin), nil)
	if err != nil {
		t.Fatalf("launch bg plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c
}

// waitState polls Status until it reports want or the deadline elapses. The
// blocking Run in the subprocess flips running true/false asynchronously, so
// the host observes the transition with a small lag.
func waitState(t *testing.T, c *host.BgClient, want string) ext.ServiceStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last ext.ServiceStatus
	for time.Now().Before(deadline) {
		st, err := c.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		last = st
		if st.State == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Status.State = %q, want %q (last=%+v)", last.State, want, last)
	return last
}

// TestBackgroundServiceGRPCRoundTrip is the FRP-external-plugin gRPC round-trip
// gate: drive the Start/Status/Stop lifecycle against a real go-plugin
// subprocess through the host BgClient (which models the blocking Run as
// Start + block-on-ctx + Stop) and confirm the three states round-trip and the
// impl's Run context is cancelled by Stop.
func TestBackgroundServiceGRPCRoundTrip(t *testing.T) {
	c := launchBgPlugin(t)
	ctx := context.Background()

	if c.Manifest().ID != "com.example.testbg" {
		t.Fatalf("manifest id = %q", c.Manifest().ID)
	}

	// Before Start, the service is stopped.
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("Status before start: %v", err)
	}
	if st.State != ext.ServiceStateStopped {
		t.Fatalf("initial state = %q, want stopped", st.State)
	}

	// Run models Start + block-until-cancel + Stop: run it in a goroutine and
	// cancel it to exercise the graceful stop path.
	runCtx, cancel := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(runCtx) }()

	// After Start reaches the impl, Status reports running with metrics.
	running := waitState(t, c, ext.ServiceStateRunning)
	if running.Detail != "synthetic tunnel" {
		t.Fatalf("running detail = %q", running.Detail)
	}
	if running.Metrics == nil || running.Metrics["starts"] == nil {
		t.Fatalf("running status must carry metrics, got %+v", running.Metrics)
	}

	// Cancelling Run's context must issue Stop, which cancels the impl's Run
	// context so the subprocess flips back to stopped.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	waitState(t, c, ext.ServiceStateStopped)
}

// TestBackgroundServiceGRPCStatusErrorPropagates confirms a plugin-side error
// status (forced via BGPLUGIN_FORCE_ERROR) surfaces through the gRPC boundary
// as a plain error carrying the ErrorProto code, not a silently empty status.
func TestBackgroundServiceGRPCStatusErrorPropagates(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	t.Setenv("BGPLUGIN_FORCE_ERROR", "1")
	c, err := host.LaunchBackgroundService(context.Background(), exec.Command(bgPluginBin), nil)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("Status against a forced-error plugin must return an error")
	} else if code, ok := ext.CodeOf(err); !ok || code != ext.ErrCodeNotSupport {
		t.Fatalf("Status error must round-trip its code, got %v (code=%q ok=%v)", err, code, ok)
	}
}
