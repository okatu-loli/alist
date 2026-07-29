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

// buildSearchIndexerPlugin compiles the gRPC round-trip test's
// searchindexerplugin fixture to a temp path and returns it, mirroring
// buildOfflineDownloadToolPlugin.
func buildSearchIndexerPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "searchindexerplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/searchindexerplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc search indexer: " + fmt.Sprintf("build searchindexerplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc search indexer: built binary missing: " + err.Error())
	}
	return out
}

func TestAcquireSearchIndexerReusesProcessAndRefcounts(t *testing.T) {
	bin := buildSearchIndexerPlugin(t)
	m := New()

	c1, rel1, err := m.AcquireSearchIndexer(context.Background(), "com.example.testsearchindexer", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.SearchIndexerCount() != 1 {
		t.Fatalf("count=%d want 1", m.SearchIndexerCount())
	}

	c2, rel2, err := m.AcquireSearchIndexer(context.Background(), "com.example.testsearchindexer", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if c1 != c2 {
		t.Fatal("same plugin id should reuse the same client")
	}
	if m.SearchIndexerCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.SearchIndexerCount())
	}

	// Config/Index/Search work end to end over the real subprocess.
	if cfg := c1.Config(); cfg.Name != "echoindexer" {
		t.Fatalf("Config().Name = %q, want echoindexer", cfg.Name)
	}
	if err := c1.Index(context.Background(), ext.SearchNode{Parent: "/a", Name: "b.txt", Size: 1}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	nodes, total, err := c1.Search(context.Background(), ext.SearchReq{Keywords: "b.txt"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total != 1 || len(nodes) != 1 || nodes[0].Name != "b.txt" {
		t.Fatalf("Search(b.txt) = %+v total=%d, want one b.txt hit", nodes, total)
	}

	rel1()
	if m.SearchIndexerCount() != 1 {
		t.Fatalf("count=%d want 1 after one release", m.SearchIndexerCount())
	}
	rel2()
	if m.SearchIndexerCount() != 0 {
		t.Fatalf("count=%d want 0 after all releases", m.SearchIndexerCount())
	}

	// release is idempotent: calling a spent closure again must not underflow.
	rel1()
	if m.SearchIndexerCount() != 0 {
		t.Fatalf("count=%d want 0 after redundant release", m.SearchIndexerCount())
	}
}

// killSearchIndexerAndWaitExited SIGKILLs c's subprocess (simulating a crash)
// and blocks until c.Exited() reports it, so callers don't race go-plugin's
// reaper. Mirrors killOfflineDownloadToolAndWaitExited.
func killSearchIndexerAndWaitExited(t *testing.T, c *host.SearchIndexerClient) {
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

// TestAcquireSearchIndexerRelaunchesDeadProcess pins the
// self-heal-on-cache-hit path: a subprocess that died between the last
// release and the next Acquire must be relaunched, not handed back as a
// corpse.
func TestAcquireSearchIndexerRelaunchesDeadProcess(t *testing.T) {
	bin := buildSearchIndexerPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.AcquireSearchIndexer(ctx, "com.example.testsearchindexer", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1, ok := c1.(*host.SearchIndexerClient)
	if !ok {
		t.Fatalf("client is %T, want *host.SearchIndexerClient", c1)
	}
	killSearchIndexerAndWaitExited(t, hc1)

	c2, rel2, err := m.AcquireSearchIndexer(ctx, "com.example.testsearchindexer", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if m.SearchIndexerCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.SearchIndexerCount())
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if cfg := c2.Config(); cfg.Name != "echoindexer" {
		t.Fatalf("Config against relaunched client = %+v", cfg)
	}

	rel1()
	rel2()
	if m.SearchIndexerCount() != 0 {
		t.Fatalf("count=%d want 0 after both releases", m.SearchIndexerCount())
	}
}

func TestAcquireSearchIndexerDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildSearchIndexerPlugin(t)
	m := New()

	_, relA, err := m.AcquireSearchIndexer(context.Background(), "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.AcquireSearchIndexer(context.Background(), "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.SearchIndexerCount() != 2 {
		t.Fatalf("count=%d want 2", m.SearchIndexerCount())
	}

	relA()
	if m.SearchIndexerCount() != 1 {
		t.Fatalf("count=%d want 1 after releasing a", m.SearchIndexerCount())
	}
	relB()
	if m.SearchIndexerCount() != 0 {
		t.Fatalf("count=%d want 0 after releasing b", m.SearchIndexerCount())
	}
}
