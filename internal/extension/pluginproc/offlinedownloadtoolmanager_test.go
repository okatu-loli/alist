package pluginproc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// buildOfflineDownloadToolPlugin compiles the gRPC round-trip test's
// offlinedownloadtoolplugin fixture to a temp path and returns it, mirroring
// buildAPIRoutePlugin.
func buildOfflineDownloadToolPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "offlinedownloadtoolplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/offlinedownloadtoolplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc offline download tool: " + fmt.Sprintf("build offlinedownloadtoolplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc offline download tool: built binary missing: " + err.Error())
	}
	return out
}

func TestAcquireOfflineDownloadToolReusesProcessAndRefcounts(t *testing.T) {
	bin := buildOfflineDownloadToolPlugin(t)
	m := New()

	c1, rel1, err := m.AcquireOfflineDownloadTool(context.Background(), "com.example.testofflinedownloadtool", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.OfflineDownloadToolCount() != 1 {
		t.Fatalf("count=%d want 1", m.OfflineDownloadToolCount())
	}

	c2, rel2, err := m.AcquireOfflineDownloadTool(context.Background(), "com.example.testofflinedownloadtool", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if c1 != c2 {
		t.Fatal("same plugin id should reuse the same client")
	}
	if m.OfflineDownloadToolCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.OfflineDownloadToolCount())
	}

	// Config/AddURL work end to end over the real subprocess.
	if cfg := c1.Config(); cfg.Name != "echotool" {
		t.Fatalf("Config().Name = %q, want echotool", cfg.Name)
	}
	gid, err := c1.AddURL(context.Background(), ext.AddArgs{URL: "u", TempDir: "t", UID: "id"})
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if want := "gid:u:t:id"; gid != want {
		t.Fatalf("AddURL gid = %q, want %q", gid, want)
	}

	rel1()
	if m.OfflineDownloadToolCount() != 1 {
		t.Fatalf("count=%d want 1 after one release", m.OfflineDownloadToolCount())
	}
	rel2()
	if m.OfflineDownloadToolCount() != 0 {
		t.Fatalf("count=%d want 0 after all releases", m.OfflineDownloadToolCount())
	}

	// release is idempotent: calling a spent closure again must not underflow.
	rel1()
	if m.OfflineDownloadToolCount() != 0 {
		t.Fatalf("count=%d want 0 after redundant release", m.OfflineDownloadToolCount())
	}
}

// killOfflineDownloadToolAndWaitExited SIGKILLs c's subprocess (simulating a
// crash) and blocks until c.Exited() reports it, so callers don't race
// go-plugin's reaper. Mirrors killAPIRouteAndWaitExited.
func killOfflineDownloadToolAndWaitExited(t *testing.T, c *host.OfflineDownloadToolClient) {
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

// TestAcquireOfflineDownloadToolRelaunchesDeadProcess pins the
// self-heal-on-cache-hit path: a subprocess that died between the last
// release and the next Acquire must be relaunched, not handed back as a
// corpse.
func TestAcquireOfflineDownloadToolRelaunchesDeadProcess(t *testing.T) {
	bin := buildOfflineDownloadToolPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.AcquireOfflineDownloadTool(ctx, "com.example.testofflinedownloadtool", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1, ok := c1.(*host.OfflineDownloadToolClient)
	if !ok {
		t.Fatalf("client is %T, want *host.OfflineDownloadToolClient", c1)
	}
	killOfflineDownloadToolAndWaitExited(t, hc1)

	c2, rel2, err := m.AcquireOfflineDownloadTool(ctx, "com.example.testofflinedownloadtool", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if m.OfflineDownloadToolCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.OfflineDownloadToolCount())
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if cfg := c2.Config(); cfg.Name != "echotool" {
		t.Fatalf("Config against relaunched client = %+v", cfg)
	}

	rel1()
	rel2()
	if m.OfflineDownloadToolCount() != 0 {
		t.Fatalf("count=%d want 0 after both releases", m.OfflineDownloadToolCount())
	}
}

func TestAcquireOfflineDownloadToolDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildOfflineDownloadToolPlugin(t)
	m := New()

	_, relA, err := m.AcquireOfflineDownloadTool(context.Background(), "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.AcquireOfflineDownloadTool(context.Background(), "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.OfflineDownloadToolCount() != 2 {
		t.Fatalf("count=%d want 2", m.OfflineDownloadToolCount())
	}

	relA()
	if m.OfflineDownloadToolCount() != 1 {
		t.Fatalf("count=%d want 1 after releasing a", m.OfflineDownloadToolCount())
	}
	relB()
	if m.OfflineDownloadToolCount() != 0 {
		t.Fatalf("count=%d want 0 after releasing b", m.OfflineDownloadToolCount())
	}
}
