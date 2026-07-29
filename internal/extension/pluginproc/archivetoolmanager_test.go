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

// archiveToolLingerTestDelay is the shrunk archiveToolIdleLinger used by
// these tests so they don't sleep the real 30s production default. Mirrors
// assistLingerTestDelay.
const archiveToolLingerTestDelay = 150 * time.Millisecond

// buildArchiveToolPlugin compiles the gRPC round-trip test's
// archivetoolplugin fixture to a temp path and returns it, mirroring
// buildSearchIndexerPlugin.
func buildArchiveToolPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "archivetoolplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/archivetoolplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc archive tool: " + fmt.Sprintf("build archivetoolplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc archive tool: built binary missing: " + err.Error())
	}
	return out
}

// killArchiveToolAndWaitExited SIGKILLs c's subprocess (simulating a crash)
// and blocks until c.Exited() reports it, so callers don't race go-plugin's
// reaper. Mirrors killAssistAndWaitExited.
func killArchiveToolAndWaitExited(t *testing.T, c *host.ArchiveToolClient) {
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

// TestArchiveToolReleaseLingersThenReusesOnAcquire pins the idle-linger's
// whole point (review fix 3, mirroring TestAssistReleaseLingersThenReusesOnAcquire):
// releasing the last reference to an archive-tool subprocess must not kill it
// right away -- archive browsing is Meta, then several List calls, then an
// Extract, each its own released-immediately HTTP request -- a re-Acquire
// within the linger window must reuse the exact same client (no second
// Launch), and the pending timer must not fire once that reuse happens.
func TestArchiveToolReleaseLingersThenReusesOnAcquire(t *testing.T) {
	bin := buildArchiveToolPlugin(t)
	m := New()
	m.archiveToolIdleLinger = archiveToolLingerTestDelay
	ctx := context.Background()

	c1, rel1, err := m.AcquireArchiveTool(ctx, "com.example.testarchivetool", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1", m.ArchiveToolCount())
	}

	rel1()
	// Still alive immediately after release: the linger just started.
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1 right after release (idle linger should keep it warm)", m.ArchiveToolCount())
	}

	c2, rel2, err := m.AcquireArchiveTool(ctx, "com.example.testarchivetool", bin)
	if err != nil {
		t.Fatalf("acquire2 within linger: %v", err)
	}
	if c1 != c2 {
		t.Fatal("re-Acquire within the linger window must reuse the same client, not launch a new one")
	}
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.ArchiveToolCount())
	}

	// Prove the reused process still works, and that the (should-be-disarmed)
	// original timer never fires and tears it down out from under this ref.
	if exts := c2.Exts(); len(exts) != 1 || exts[0] != "fx" {
		t.Fatalf("Exts against reused client = %v, want [fx]", exts)
	}
	meta, err := c2.Meta(ctx, ext.ArchiveSource{Path: "/unused", Size: 11}, "")
	if err != nil {
		t.Fatalf("Meta against reused client: %v", err)
	}
	if meta.EntryCount != 2 {
		t.Fatalf("Meta.EntryCount = %d, want 2", meta.EntryCount)
	}
	time.Sleep(archiveToolLingerTestDelay + 100*time.Millisecond)
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1: the disarmed linger timer must not have killed the reused, still-referenced process", m.ArchiveToolCount())
	}
	hc2 := c2.(*host.ArchiveToolClient)
	if hc2.Exited() {
		t.Fatal("reused process must still be alive (disarmed timer fired anyway)")
	}

	rel2()
}

// TestArchiveToolReleaseShutsDownAfterLingerTimeout is
// TestArchiveToolReleaseLingersThenReusesOnAcquire's other half: once the
// linger window actually elapses with no re-Acquire, the process must be
// shut down and the pool entry removed. Mirrors
// TestAssistReleaseShutsDownAfterLingerTimeout.
func TestArchiveToolReleaseShutsDownAfterLingerTimeout(t *testing.T) {
	bin := buildArchiveToolPlugin(t)
	m := New()
	m.archiveToolIdleLinger = archiveToolLingerTestDelay
	ctx := context.Background()

	c1, rel1, err := m.AcquireArchiveTool(ctx, "com.example.testarchivetool", bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	hc1, ok := c1.(*host.ArchiveToolClient)
	if !ok {
		t.Fatalf("client is %T, want *host.ArchiveToolClient", c1)
	}

	rel1()
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1 immediately after release", m.ArchiveToolCount())
	}

	deadline := time.Now().Add(5 * time.Second)
	for m.ArchiveToolCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("count=%d want 0 after the idle linger elapsed", m.ArchiveToolCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The pool entry is removed inside the linger callback's critical section,
	// but Shutdown (go-plugin's Kill) now deliberately runs AFTER that section
	// releases m.mu, so ArchiveToolCount() can already read 0 for a brief
	// window before Exited() flips. Poll it too instead of asserting
	// immediately.
	for !hc1.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("process must have been shut down once the idle linger elapsed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestArchiveToolLingerDoesNotMisfireAcrossRelaunchGeneration pins the
// generation/refs guard: if the subprocess crashes and gets relaunched
// (self-heal-on-cache-hit) while the OLD generation's linger timer is still
// pending, that stale timer must never tear down the NEW generation's
// process. Mirrors TestAssistLingerDoesNotMisfireAcrossRelaunchGeneration.
func TestArchiveToolLingerDoesNotMisfireAcrossRelaunchGeneration(t *testing.T) {
	bin := buildArchiveToolPlugin(t)
	m := New()
	m.archiveToolIdleLinger = archiveToolLingerTestDelay
	ctx := context.Background()

	c1, rel1, err := m.AcquireArchiveTool(ctx, "com.example.testarchivetool", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1 := c1.(*host.ArchiveToolClient)

	// Drop to zero refs: arms the gen-0 linger timer.
	rel1()

	// Crash the gen-0 process and relaunch it via a fresh Acquire, well within
	// the linger window -- this must disarm the gen-0 timer and hand back a
	// gen-1 process.
	killArchiveToolAndWaitExited(t, hc1)
	c2, rel2, err := m.AcquireArchiveTool(ctx, "com.example.testarchivetool", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.ArchiveToolCount())
	}
	if exts := c2.Exts(); len(exts) != 1 || exts[0] != "fx" {
		t.Fatalf("Exts against relaunched client = %v, want [fx]", exts)
	}

	// Hold this reference past the ORIGINAL gen-0 timer's schedule: if that
	// stale timer misfired against the new generation, this would now be
	// dead/gone.
	time.Sleep(archiveToolLingerTestDelay + 100*time.Millisecond)
	if m.ArchiveToolCount() != 1 {
		t.Fatalf("count=%d want 1: the stale gen-0 linger must not have killed the referenced gen-1 process", m.ArchiveToolCount())
	}
	hc2 := c2.(*host.ArchiveToolClient)
	if hc2.Exited() {
		t.Fatal("gen-1 process must still be alive; a stale linger timer killed it")
	}

	// The new generation's own linger must still work normally once it too
	// drops to zero refs.
	rel2()
	deadline := time.Now().Add(5 * time.Second)
	for m.ArchiveToolCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("count=%d want 0 after gen-1's own idle linger elapsed", m.ArchiveToolCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAcquireArchiveToolDifferentPluginIDsGetSeparateProcesses confirms two
// distinct pluginIDs never share a subprocess, and that each's own idle
// linger runs independently of the other's.
func TestAcquireArchiveToolDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildArchiveToolPlugin(t)
	m := New()
	m.archiveToolIdleLinger = archiveToolLingerTestDelay
	ctx := context.Background()

	_, relA, err := m.AcquireArchiveTool(ctx, "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.AcquireArchiveTool(ctx, "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.ArchiveToolCount() != 2 {
		t.Fatalf("count=%d want 2", m.ArchiveToolCount())
	}

	relA()
	relB()
	deadline := time.Now().Add(5 * time.Second)
	for m.ArchiveToolCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("count=%d want 0 once both idle lingers elapsed", m.ArchiveToolCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
