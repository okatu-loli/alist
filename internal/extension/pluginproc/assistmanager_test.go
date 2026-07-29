package pluginproc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// assistLingerTestDelay is the shrunk assistIdleLinger used by these tests so
// they don't sleep the real 30s production default. Kept generous enough
// (relative to killAssistAndWaitExited's typical sub-millisecond kill+detect
// latency) that a slow CI box doesn't make the "reuse within linger" or
// "relaunch during linger" assertions flaky.
const assistLingerTestDelay = 150 * time.Millisecond

// killAssistAndWaitExited SIGKILLs c's subprocess (simulating a crash) and
// blocks until c.Exited() reports it. Mirrors killActionAndWaitExited one
// client type over.
func killAssistAndWaitExited(t *testing.T, c *host.AssistClient) {
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
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAssistReleaseLingersThenReusesOnAcquire pins the idle-linger's whole
// point: releasing the last reference to an assist subprocess must not kill
// it right away — a re-Acquire within the linger window must reuse the exact
// same client (no second Launch), and the pending timer must not fire once
// that reuse happens.
func TestAssistReleaseLingersThenReusesOnAcquire(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.assistIdleLinger = assistLingerTestDelay
	ctx := context.Background()

	c1, rel1, err := m.AcquireAssist(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1", m.AssistCount())
	}

	rel1()
	// Still alive immediately after release: the linger just started.
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1 right after release (idle linger should keep it warm)", m.AssistCount())
	}

	c2, rel2, err := m.AcquireAssist(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire2 within linger: %v", err)
	}
	if c1 != c2 {
		t.Fatal("re-Acquire within the linger window must reuse the same client, not launch a new one")
	}
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.AssistCount())
	}

	// Prove the reused process still works, and that the (should-be-disarmed)
	// original timer never fires and tears it down out from under this ref.
	if _, err := c2.Assist(ctx, "", "probe", nil); err != nil {
		t.Fatalf("Assist against reused client: %v", err)
	}
	time.Sleep(assistLingerTestDelay + 100*time.Millisecond)
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1: the disarmed linger timer must not have killed the reused, still-referenced process", m.AssistCount())
	}
	hc2 := c2.(*host.AssistClient)
	if hc2.Exited() {
		t.Fatal("reused process must still be alive (disarmed timer fired anyway)")
	}

	rel2()
}

// TestAssistReleaseShutsDownAfterLingerTimeout pins the other half: once the
// linger window actually elapses with no re-Acquire, the process must be
// shut down and the pool entry removed.
func TestAssistReleaseShutsDownAfterLingerTimeout(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.assistIdleLinger = assistLingerTestDelay
	ctx := context.Background()

	c1, rel1, err := m.AcquireAssist(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	hc1, ok := c1.(*host.AssistClient)
	if !ok {
		t.Fatalf("client is %T, want *host.AssistClient", c1)
	}

	rel1()
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1 immediately after release", m.AssistCount())
	}

	deadline := time.Now().Add(5 * time.Second)
	for m.AssistCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("count=%d want 0 after the idle linger elapsed", m.AssistCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The pool entry is removed inside the linger callback's critical section,
	// but Shutdown (go-plugin's Kill, which can block on the subprocess's own
	// exit) now deliberately runs AFTER that section releases m.mu (see
	// scheduleAssistLinger's "Shutdown must run outside the lock" invariant)
	// — so AssistCount() can already read 0 for a brief window before Exited()
	// flips. Poll it too instead of asserting immediately.
	for !hc1.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("process must have been shut down once the idle linger elapsed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAssistLingerDoesNotMisfireAcrossRelaunchGeneration pins the
// generation/refs guard: if the subprocess crashes and gets relaunched
// (self-heal-on-cache-hit) while the OLD generation's linger timer is still
// pending, that stale timer must never tear down the NEW generation's
// process — whether or not it managed to Stop() cleanly. The new generation
// must still linger-and-shutdown normally on its own schedule afterward.
func TestAssistLingerDoesNotMisfireAcrossRelaunchGeneration(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.assistIdleLinger = assistLingerTestDelay
	ctx := context.Background()

	c1, rel1, err := m.AcquireAssist(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1 := c1.(*host.AssistClient)

	// Drop to zero refs: arms the gen-0 linger timer.
	rel1()

	// Crash the gen-0 process and relaunch it via a fresh Acquire, well within
	// the linger window — this must disarm the gen-0 timer and hand back a
	// gen-1 process.
	killAssistAndWaitExited(t, hc1)
	c2, rel2, err := m.AcquireAssist(ctx, "com.example.testaction", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.AssistCount())
	}

	// Hold this reference past the ORIGINAL gen-0 timer's schedule: if that
	// stale timer misfired against the new generation, this would now be
	// dead/gone.
	time.Sleep(assistLingerTestDelay + 100*time.Millisecond)
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1: the stale gen-0 linger must not have killed the referenced gen-1 process", m.AssistCount())
	}
	hc2 := c2.(*host.AssistClient)
	if hc2.Exited() {
		t.Fatal("gen-1 process must still be alive; a stale linger timer killed it")
	}

	// The new generation's own linger must still work normally once it too
	// drops to zero refs.
	rel2()
	deadline := time.Now().Add(5 * time.Second)
	for m.AssistCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("count=%d want 0 after gen-1's own idle linger elapsed", m.AssistCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
