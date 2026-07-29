package pluginproc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// buildBgPlugin compiles the gRPC round-trip test's bgplugin fixture to a temp
// path and returns it, mirroring buildURLPlugin. It skips the test if the go
// toolchain build fails.
func buildBgPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "bgplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/bgplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc bg: " + fmt.Sprintf("build bgplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc bg: built binary missing: " + err.Error())
	}
	return out
}

func TestAcquireBackgroundServiceReusesProcessAndRefcounts(t *testing.T) {
	bin := buildBgPlugin(t)
	m := New()

	c1, rel1, err := m.AcquireBackgroundService(context.Background(), "com.example.testbg", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.BgCount() != 1 {
		t.Fatalf("count=%d want 1", m.BgCount())
	}

	c2, rel2, err := m.AcquireBackgroundService(context.Background(), "com.example.testbg", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if c1 != c2 {
		t.Fatal("same plugin id should reuse the same client")
	}
	if m.BgCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.BgCount())
	}

	rel1()
	if m.BgCount() != 1 {
		t.Fatalf("count=%d want 1 after one release", m.BgCount())
	}
	rel2()
	if m.BgCount() != 0 {
		t.Fatalf("count=%d want 0 after all releases", m.BgCount())
	}

	// release is idempotent: calling a spent closure again must not underflow.
	rel1()
	if m.BgCount() != 0 {
		t.Fatalf("count=%d want 0 after redundant release", m.BgCount())
	}
}

// killBgAndWaitExited SIGKILLs c's subprocess (simulating a crash) and blocks
// until c.Exited() reports it, so callers don't race go-plugin's reaper.
func killBgAndWaitExited(t *testing.T, c *host.BgClient) {
	t.Helper()
	pid := c.Pid()
	if pid == 0 {
		t.Fatal("client has no pid (process not started?)")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill process %d: %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !c.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("client did not report Exited() after kill -9")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAcquireBackgroundServiceRelaunchesDeadProcess pins the
// self-heal-on-cache-hit path: a subprocess that died between the last release
// and the next Acquire must be relaunched, not handed back as a corpse.
func TestAcquireBackgroundServiceRelaunchesDeadProcess(t *testing.T) {
	bin := buildBgPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.AcquireBackgroundService(ctx, "com.example.testbg", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1, ok := c1.(*host.BgClient)
	if !ok {
		t.Fatalf("client is %T, want *host.BgClient", c1)
	}
	killBgAndWaitExited(t, hc1)

	c2, rel2, err := m.AcquireBackgroundService(ctx, "com.example.testbg", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if m.BgCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.BgCount())
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if _, err := c2.Status(ctx); err != nil {
		t.Fatalf("Status against relaunched client: %v", err)
	}

	rel1()
	rel2()
	if m.BgCount() != 0 {
		t.Fatalf("count=%d want 0 after both releases", m.BgCount())
	}
}

func TestAcquireBackgroundServiceDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildBgPlugin(t)
	m := New()

	_, relA, err := m.AcquireBackgroundService(context.Background(), "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.AcquireBackgroundService(context.Background(), "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.BgCount() != 2 {
		t.Fatalf("count=%d want 2", m.BgCount())
	}

	relA()
	if m.BgCount() != 1 {
		t.Fatalf("count=%d want 1 after releasing a", m.BgCount())
	}
	relB()
	if m.BgCount() != 0 {
		t.Fatalf("count=%d want 0 after releasing b", m.BgCount())
	}
}
