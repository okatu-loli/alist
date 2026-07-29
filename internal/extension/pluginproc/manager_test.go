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
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
)

// buildURLPlugin compiles the gRPC parity test's urlplugin fixture to a temp
// path and returns it, mirroring internal/extension/grpc/parity_test.go's
// buildFixtures helper. It skips the test if the go toolchain build fails.
func buildURLPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "urlplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urlplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip pluginproc: " + fmt.Sprintf("build urlplugin: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip pluginproc: built binary missing: " + err.Error())
	}
	return out
}

func TestAcquireReusesProcessAndRefcounts(t *testing.T) {
	bin := buildURLPlugin(t)
	m := New()

	c1, rel1, err := m.Acquire(context.Background(), "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1", m.Count())
	}

	c2, rel2, err := m.Acquire(context.Background(), "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if c1 != c2 {
		t.Fatal("same plugin id should reuse the same client")
	}
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after reuse", m.Count())
	}

	rel1()
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after one release", m.Count())
	}

	rel2()
	if m.Count() != 0 {
		t.Fatalf("count=%d want 0 after all releases", m.Count())
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	bin := buildURLPlugin(t)
	m := New()

	_, rel1, err := m.Acquire(context.Background(), "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	_, _, err = m.Acquire(context.Background(), "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire2: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after two acquires", m.Count())
	}

	rel1()
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after first release", m.Count())
	}

	// Calling the same release closure again must be a no-op: it must not
	// decrement refs a second time and must not tear down the still-live
	// process backing the second, un-released Acquire.
	rel1()
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after redundant release call (release must be idempotent)", m.Count())
	}
}

// killAndWaitExited sends SIGKILL to c's subprocess (simulating a crash —
// panic, OOM kill, segfault: anything that ends the process without a clean
// go-plugin shutdown handshake) and blocks until c.Exited() reports it, so
// callers don't race go-plugin's own process-reaper goroutine.
func killAndWaitExited(t *testing.T, c *host.Client) {
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

// TestAcquireRelaunchesDeadProcess pins the lazy-restart-on-Acquire path: a
// plugin subprocess that died between the last release and the next Acquire
// (nobody made a call in between to trigger bridge.pluginDriver's own
// crash-retry path) must not be handed out as-is — Acquire must notice via
// Exited() and relaunch it before returning, so a fresh/returning mount
// never starts against a corpse.
func TestAcquireRelaunchesDeadProcess(t *testing.T) {
	bin := buildURLPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.Acquire(ctx, "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	hc1, ok := c1.(*host.Client)
	if !ok {
		t.Fatalf("client is %T, want *host.Client", c1)
	}
	killAndWaitExited(t, hc1)

	// A second acquirer (e.g. a new storage mounted on the same plugin ID, or
	// this same one being Init'd again) must get a live client back, not the
	// corpse — and the process count must stay at exactly one live process
	// (no leak of the dead one alongside a new one).
	c2, rel2, err := m.Acquire(ctx, "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire2 after crash: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after relaunch", m.Count())
	}
	if c2 == c1 {
		t.Fatal("relaunch must replace the dead client, not hand back the corpse")
	}
	if _, err := c2.List(ctx, ext.ObjRef{}, ext.ListArgs{}); err != nil {
		t.Fatalf("List against relaunched client: %v", err)
	}

	rel1()
	rel2()
	if m.Count() != 0 {
		t.Fatalf("count=%d want 0 after both releases", m.Count())
	}
}

// TestRestartReturnsLiveClient pins Restart's happy path: given the client a
// caller observed as dead, Restart relaunches the subprocess and the
// returned client actually works.
func TestRestartReturnsLiveClient(t *testing.T) {
	bin := buildURLPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.Acquire(ctx, "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer rel1()
	hc1 := c1.(*host.Client)
	killAndWaitExited(t, hc1)

	fresh, err := m.Restart(ctx, "com.example.demo", bin, c1)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if fresh == c1 {
		t.Fatal("restart must return a new client, not the dead one")
	}
	if _, err := fresh.List(ctx, ext.ObjRef{}, ext.ListArgs{}); err != nil {
		t.Fatalf("List against restarted client: %v", err)
	}
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 (single process, not leaked)", m.Count())
	}
}

// TestRestartCollapsesConcurrentCallers pins the concurrency guard: when two
// storages sharing one pluginID both observe the same dead client and race
// to call Restart, only one relaunch must happen — the loser must get back
// the winner's fresh client rather than relaunching a second (redundant,
// wasteful, and racy) subprocess.
func TestRestartCollapsesConcurrentCallers(t *testing.T) {
	bin := buildURLPlugin(t)
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.Acquire(ctx, "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer rel1()
	hc1 := c1.(*host.Client)
	killAndWaitExited(t, hc1)

	const n = 8
	results := make([]ext.StorageDriver, n)
	errs := make([]error, n)
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			results[i], errs[i] = m.Restart(ctx, "com.example.demo", bin, c1)
			done <- i
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("restart[%d]: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent restarts returned different clients (want a single collapsed relaunch): [0]=%v [%d]=%v", results[0], i, results[i])
		}
	}
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 (concurrent restarts must collapse into a single subprocess)", m.Count())
	}
}

// TestRestartFailureReturnsErrorWithoutLooping pins the failure path: if the
// relaunch itself cannot succeed (binary gone), Restart must return a plain
// error — not panic, not retry forever — and must not leave the manager
// claiming a live process that doesn't exist.
func TestRestartFailureReturnsErrorWithoutLooping(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "urlplugin")
	fixtureData, err := os.ReadFile(buildURLPlugin(t))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(bin, fixtureData, 0o755); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.Acquire(ctx, "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer rel1()
	hc1 := c1.(*host.Client)
	killAndWaitExited(t, hc1)

	// Remove the binary so the relaunch itself fails.
	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove binary: %v", err)
	}

	if _, err := m.Restart(ctx, "com.example.demo", bin, c1); err == nil {
		t.Fatal("restart with a missing binary must return an error")
	}
	// The failed restart must not have wedged the entry into a bad state that
	// a later, valid restart (e.g. once the binary is reinstated) can't
	// recover from.
	if err := os.WriteFile(bin, fixtureData, 0o755); err != nil {
		t.Fatalf("restore binary: %v", err)
	}
	if _, err := m.Restart(ctx, "com.example.demo", bin, c1); err != nil {
		t.Fatalf("restart after binary restored: %v", err)
	}
}

// TestAcquireFailureReturnsLiteralNilNotTypedNil pins review round 1's fix
// for a typed-nil-interface trap the generic pool[C] refactor introduced:
// pool.acquire's own error path returns `var zero C` — for every C here that
// is a nil *host.XxxClient POINTER, not an interface. Manager's wrappers
// used to forward that value straight through
// (`return m.xxxProcs.acquire(...)`), and Go boxes a concrete nil pointer
// into an interface-typed return as a NON-nil interface (dynamic type set,
// dynamic value nil — the classic typed-nil trap): every caller's `driver !=
// nil` check then sees true and goes on to call methods on a nil pointer.
// Each wrapper must instead check the error explicitly and
// `return nil, nil, err` — a literal, untyped nil converted straight to a
// genuinely nil interface.
//
// Exercises one non-lingering pool (Acquire/StorageDriver), one lingering
// pool (AcquireAssist), and one more non-lingering pool (AcquireAction) — a
// nonexistent binary path fails host.Launch immediately (no subprocess ever
// starts), so this needs no fixture build and runs instantly.
func TestAcquireFailureReturnsLiteralNilNotTypedNil(t *testing.T) {
	m := New()
	ctx := context.Background()
	bogusBin := filepath.Join(t.TempDir(), "does-not-exist")

	if c, release, err := m.Acquire(ctx, "com.example.bogus", bogusBin); err == nil {
		t.Fatal("Acquire against a nonexistent binary must fail")
	} else if c != nil {
		t.Fatalf("Acquire error path: client = %#v, want a literal nil interface (typed-nil trap)", c)
	} else if release != nil {
		t.Fatal("Acquire error path: release must be nil")
	}

	if c, _, err := m.AcquireAction(ctx, "com.example.bogus", bogusBin); err == nil {
		t.Fatal("AcquireAction against a nonexistent binary must fail")
	} else if c != nil {
		t.Fatalf("AcquireAction error path: client = %#v, want a literal nil interface (typed-nil trap)", c)
	}

	if c, _, err := m.AcquireAssist(ctx, "com.example.bogus", bogusBin); err == nil {
		t.Fatal("AcquireAssist against a nonexistent binary must fail")
	} else if c != nil {
		t.Fatalf("AcquireAssist error path: client = %#v, want a literal nil interface (typed-nil trap)", c)
	}
}

// TestRestartFailureReturnsLiteralNilNotTypedNil is
// TestRestartFailureReturnsErrorWithoutLooping's counterpart for the same
// typed-nil trap, on Restart's error path specifically (TestRestart
// FailureReturnsErrorWithoutLooping never asserted on the returned client,
// only the error).
func TestRestartFailureReturnsLiteralNilNotTypedNil(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "urlplugin")
	fixtureData, err := os.ReadFile(buildURLPlugin(t))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(bin, fixtureData, 0o755); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	m := New()
	ctx := context.Background()

	c1, rel1, err := m.Acquire(ctx, "com.example.demo", bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer rel1()
	hc1 := c1.(*host.Client)
	killAndWaitExited(t, hc1)

	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove binary: %v", err)
	}

	fresh, err := m.Restart(ctx, "com.example.demo", bin, c1)
	if err == nil {
		t.Fatal("restart with a missing binary must return an error")
	}
	if fresh != nil {
		t.Fatalf("restart error path: client = %#v, want a literal nil interface (typed-nil trap)", fresh)
	}
}

// TestEvictNoopWhenNothingCached pins Evict's no-op path: nothing cached for
// pluginID in any pool means there is nothing to relaunch, so it must return
// nil without launching anything.
func TestEvictNoopWhenNothingCached(t *testing.T) {
	m := New()
	if err := m.Evict(context.Background(), "com.example.nothing-cached", "/no/such/binary"); err != nil {
		t.Fatalf("evict with nothing cached = %v, want nil (no-op)", err)
	}
}

// TestEvictRelaunchesEveryKindPoolForPluginID pins the security-relevant part
// of generalizing Evict beyond the StorageDriver pool: an admin narrowing a
// plugin's Grant (SetGrant, server/handles/plugins.go) must relaunch EVERY
// Kind's subprocess for that plugin ID, not just the StorageDriver one — the
// borrowed-capability layer closes over Grant.Network at launch, so an
// Action or Assist subprocess that was already running keeps serving under
// the OLD, wider authority forever if only the StorageDriver pool gets
// evicted. actionplugin (testdata/actionplugin) implements both ext.Action
// and ext.Assist under the same manifest ID, so acquiring it through both
// AcquireAction and AcquireAssist gives two independent, real subprocesses
// (different pools never share a process) to prove both get relaunched by a
// single Evict call.
func TestEvictRelaunchesEveryKindPoolForPluginID(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	c1a, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	defer relA()
	hc1a := c1a.(*host.ActionClient)

	c1s, relS, err := m.AcquireAssist(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire assist: %v", err)
	}
	defer relS()
	hc1s := c1s.(*host.AssistClient)

	if m.ActionCount() != 1 || m.AssistCount() != 1 {
		t.Fatalf("counts = action:%d assist:%d, want 1/1", m.ActionCount(), m.AssistCount())
	}

	if err := m.Evict(ctx, pluginID, bin); err != nil {
		t.Fatalf("evict: %v", err)
	}

	// Both Kinds' pre-evict subprocesses must have been torn down -- if Evict
	// only touched the StorageDriver pool (the bug this test guards against),
	// neither of these would ever report Exited.
	deadline := time.Now().Add(5 * time.Second)
	for !hc1a.Exited() || !hc1s.Exited() {
		if time.Now().After(deadline) {
			t.Fatalf("evict must relaunch every Kind pool for pluginID, not just StorageDriver: action exited=%v assist exited=%v", hc1a.Exited(), hc1s.Exited())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Each pool's cached entry must now be a live, working replacement --
	// still under the same reference (relA/relS above haven't run yet), so
	// this is a cache hit returning the fresh client, not a fresh Acquire.
	c2a, relA2, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action after evict: %v", err)
	}
	defer relA2()
	if c2a == c1a {
		t.Fatal("action pool must have a fresh client after Evict")
	}
	if _, err := c2a.Invoke(ctx, "", "echo", nil); err != nil {
		t.Fatalf("Invoke against evicted+relaunched action client: %v", err)
	}

	c2s, relS2, err := m.AcquireAssist(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire assist after evict: %v", err)
	}
	defer relS2()
	if c2s == c1s {
		t.Fatal("assist pool must have a fresh client after Evict")
	}
	if _, err := c2s.Assist(ctx, "", "probe", nil); err != nil {
		t.Fatalf("Assist against evicted+relaunched client: %v", err)
	}
}

// TestEvictOfIdleLingeringEntryTearsDownWithoutLeaking pins the refs==0 path
// added to pool.evict: evicting an entry that is currently idle-lingering
// (zero references, pending linger timer) must not relaunch a replacement
// process nobody references (which would never get reclaimed, since nothing
// re-arms its linger timer) -- it must disarm the stale timer and tear the
// process down immediately, same end state as if the linger had just fired.
func TestEvictOfIdleLingeringEntryTearsDownWithoutLeaking(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.assistIdleLinger = assistLingerTestDelay
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	c1, rel1, err := m.AcquireAssist(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	hc1 := c1.(*host.AssistClient)

	// Drop to zero refs: arms the idle-linger timer.
	rel1()
	if m.AssistCount() != 1 {
		t.Fatalf("count=%d want 1 immediately after release (still lingering)", m.AssistCount())
	}

	if err := m.Evict(ctx, pluginID, bin); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if m.AssistCount() != 0 {
		t.Fatalf("count=%d want 0 right after evicting an idle-lingering entry (torn down, not relaunched)", m.AssistCount())
	}

	deadline := time.Now().Add(5 * time.Second)
	for !hc1.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("evicted idle-lingering process must have been shut down")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The original linger's own deadline elapsing afterward must be a no-op:
	// there is nothing left to tear down a second time, and nothing new must
	// appear either.
	time.Sleep(assistLingerTestDelay + 100*time.Millisecond)
	if m.AssistCount() != 0 {
		t.Fatalf("count=%d want 0: the evicted entry's original linger timer must not resurrect anything", m.AssistCount())
	}
}

func TestAcquireDifferentPluginIDsGetSeparateProcesses(t *testing.T) {
	bin := buildURLPlugin(t)
	m := New()

	_, relA, err := m.Acquire(context.Background(), "com.example.a", bin)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	_, relB, err := m.Acquire(context.Background(), "com.example.b", bin)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if m.Count() != 2 {
		t.Fatalf("count=%d want 2", m.Count())
	}

	relA()
	if m.Count() != 1 {
		t.Fatalf("count=%d want 1 after releasing a", m.Count())
	}
	relB()
	if m.Count() != 0 {
		t.Fatalf("count=%d want 0 after releasing b", m.Count())
	}
}

func TestLaunchHostBuilderNilWhenUnset(t *testing.T) {
	m := New()
	if got := m.launchHostBuilder("com.example.demo"); got != nil {
		t.Fatal("launchHostBuilder with no SetHostBuilder returned non-nil func, want nil (pre-bridge behavior preserved)")
	}
}

func TestLaunchHostBuilderDelegatesToInstalledBuilder(t *testing.T) {
	m := New()
	want := hostcap.New(hostcap.Grant{PluginID: "com.example.demo"}, hostcap.Options{})
	var gotID string
	m.SetHostBuilder(func(pluginID string) ext.Host {
		gotID = pluginID
		return want
	})

	fn := m.launchHostBuilder("com.example.demo")
	if fn == nil {
		t.Fatal("launchHostBuilder = nil after SetHostBuilder, want non-nil func")
	}
	got := fn()
	if got != ext.Host(want) {
		t.Fatalf("launchHostBuilder(id)() = %v, want the host returned by the installed builder", got)
	}
	if gotID != "com.example.demo" {
		t.Fatalf("builder received pluginID = %q, want %q", gotID, "com.example.demo")
	}
}
