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

// buildAPIRoutePlugin compiles the gRPC round-trip test's apirouteplugin
// fixture to a temp path and returns it, mirroring buildBgPlugin.
func buildAPIRoutePlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "apirouteplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/apirouteplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc apiroute: " + fmt.Sprintf("build apirouteplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc apiroute: built binary missing: " + err.Error())
	}
	return out
}

func TestAcquireAPIRouteReusesProcessAndRefcounts(t *testing.T) {
	bin := buildAPIRoutePlugin(t)
	m := New()

	c1, rel1, err := m.AcquireAPIRoute(context.Background(), "com.example.testapiroute", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.APIRouteCount() != 1 {
		t.Fatalf("count=%d want 1", m.APIRouteCount())
	}

	c2, rel2, err := m.AcquireAPIRoute(context.Background(), "com.example.testapiroute", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if c1 != c2 {
		t.Fatal("same plugin id should reuse the same client")
	}
	if m.APIRouteCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.APIRouteCount())
	}

	// Routes/Handle work end to end over the real subprocess.
	decls := c1.Routes()
	if len(decls) != 2 || decls[0].Prefix != "/webhook" || decls[1].Prefix != "/s" || !decls[1].Public {
		t.Fatalf("Routes = %+v", decls)
	}

	rel1()
	if m.APIRouteCount() != 1 {
		t.Fatalf("count=%d want 1 after one release", m.APIRouteCount())
	}
	rel2()
	if m.APIRouteCount() != 0 {
		t.Fatalf("count=%d want 0 after all releases", m.APIRouteCount())
	}

	// release is idempotent: calling a spent closure again must not underflow.
	rel1()
	if m.APIRouteCount() != 0 {
		t.Fatalf("count=%d want 0 after redundant release", m.APIRouteCount())
	}
}

// killAPIRouteAndWaitExited SIGKILLs c's subprocess (simulating a crash) and
// blocks until c.Exited() reports it, so callers don't race go-plugin's
// reaper. Mirrors killBgAndWaitExited.
func killAPIRouteAndWaitExited(t *testing.T, c *host.APIRouteClient) {
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

// TestAcquireAPIRouteRelaunchesDeadProcess pins the self-heal-on-cache-hit
// path: a subprocess that died between the last release and the next Acquire
// must be relaunched, not handed back as a corpse.
func TestAcquireAPIRouteRelaunchesDeadProcess(t *testing.T) {
	bin := buildAPIRoutePlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.AcquireAPIRoute(ctx, "com.example.testapiroute", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1, ok := c1.(*host.APIRouteClient)
	if !ok {
		t.Fatalf("client is %T, want *host.APIRouteClient", c1)
	}
	killAPIRouteAndWaitExited(t, hc1)

	c2, rel2, err := m.AcquireAPIRoute(ctx, "com.example.testapiroute", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if m.APIRouteCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.APIRouteCount())
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if decls := c2.Routes(); len(decls) != 2 {
		t.Fatalf("Routes against relaunched client = %+v", decls)
	}

	rel1()
	rel2()
	if m.APIRouteCount() != 0 {
		t.Fatalf("count=%d want 0 after both releases", m.APIRouteCount())
	}
}

func TestAcquireAPIRouteDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildAPIRoutePlugin(t)
	m := New()

	_, relA, err := m.AcquireAPIRoute(context.Background(), "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.AcquireAPIRoute(context.Background(), "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.APIRouteCount() != 2 {
		t.Fatalf("count=%d want 2", m.APIRouteCount())
	}

	relA()
	if m.APIRouteCount() != 1 {
		t.Fatalf("count=%d want 1 after releasing a", m.APIRouteCount())
	}
	relB()
	if m.APIRouteCount() != 0 {
		t.Fatalf("count=%d want 0 after releasing b", m.APIRouteCount())
	}
}
