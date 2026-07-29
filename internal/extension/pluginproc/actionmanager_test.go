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

// buildActionPlugin compiles the gRPC round-trip test's actionplugin fixture
// to a temp path and returns it, mirroring buildAPIRoutePlugin.
func buildActionPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "actionplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/actionplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc action: " + fmt.Sprintf("build actionplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc action: built binary missing: " + err.Error())
	}
	return out
}

func TestAcquireActionReusesProcessAndRefcounts(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()

	c1, rel1, err := m.AcquireAction(context.Background(), "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.ActionCount() != 1 {
		t.Fatalf("count=%d want 1", m.ActionCount())
	}

	c2, rel2, err := m.AcquireAction(context.Background(), "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if c1 != c2 {
		t.Fatal("same plugin id should reuse the same client")
	}
	if m.ActionCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.ActionCount())
	}

	// Invoke works end to end over the real subprocess.
	res, err := c1.Invoke(context.Background(), "principal-x", "echo", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	m2, ok := res.(map[string]any)
	if !ok || m2["principal"] != "principal-x" || m2["action_id"] != "echo" {
		t.Fatalf("Invoke result = %+v", res)
	}
	argsOut, ok := m2["args"].(map[string]any)
	if !ok || argsOut["k"] != "v" {
		t.Fatalf("Invoke result args = %+v", m2["args"])
	}

	rel1()
	if m.ActionCount() != 1 {
		t.Fatalf("count=%d want 1 after one release", m.ActionCount())
	}
	rel2()
	if m.ActionCount() != 0 {
		t.Fatalf("count=%d want 0 after all releases", m.ActionCount())
	}

	// release is idempotent: calling a spent closure again must not underflow.
	rel1()
	if m.ActionCount() != 0 {
		t.Fatalf("count=%d want 0 after redundant release", m.ActionCount())
	}
}

// killActionAndWaitExited SIGKILLs c's subprocess (simulating a crash) and
// blocks until c.Exited() reports it, so callers don't race go-plugin's
// reaper. Mirrors killAPIRouteAndWaitExited.
func killActionAndWaitExited(t *testing.T, c *host.ActionClient) {
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

// TestAcquireActionRelaunchesDeadProcess pins the self-heal-on-cache-hit path:
// a subprocess that died between the last release and the next Acquire must be
// relaunched, not handed back as a corpse.
func TestAcquireActionRelaunchesDeadProcess(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.AcquireAction(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1, ok := c1.(*host.ActionClient)
	if !ok {
		t.Fatalf("client is %T, want *host.ActionClient", c1)
	}
	killActionAndWaitExited(t, hc1)

	c2, rel2, err := m.AcquireAction(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if m.ActionCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.ActionCount())
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if _, err := c2.Invoke(ctx, "", "echo", nil); err != nil {
		t.Fatalf("Invoke against relaunched client: %v", err)
	}

	rel1()
	rel2()
	if m.ActionCount() != 0 {
		t.Fatalf("count=%d want 0 after both releases", m.ActionCount())
	}
}

func TestAcquireActionDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()

	_, relA, err := m.AcquireAction(context.Background(), "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.AcquireAction(context.Background(), "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.ActionCount() != 2 {
		t.Fatalf("count=%d want 2", m.ActionCount())
	}

	relA()
	if m.ActionCount() != 1 {
		t.Fatalf("count=%d want 1 after releasing a", m.ActionCount())
	}
	relB()
	if m.ActionCount() != 0 {
		t.Fatalf("count=%d want 0 after releasing b", m.ActionCount())
	}
}
