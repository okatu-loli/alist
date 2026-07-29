package grpc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// launchSharedActionPlugin starts the actionplugin fixture (Action +
// EventHook + Assist, see testdata/actionplugin/main.go's G2 addition) over
// ONE shared go-plugin subprocess via host.LaunchShared, bootstrapping off
// its Action Kind.
func launchSharedActionPlugin(t *testing.T) *host.SharedClient {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	sc, err := host.LaunchShared(context.Background(), exec.Command(actionPluginBin), nil, ext.KindAction)
	if err != nil {
		t.Fatalf("launch shared action plugin: %v", err)
	}
	t.Cleanup(func() { _ = sc.Shutdown(context.Background()) })
	return sc
}

// TestSharedClientBootstrapsManifestFromPrimaryKind pins LaunchShared's
// bootstrap step: the manifest it loads off the primaryKind (Action here)
// must be the plugin's real, full manifest — both Kinds it actually
// declares (Action AND EventHook), not just the one LaunchShared happened
// to dispense to get there.
func TestSharedClientBootstrapsManifestFromPrimaryKind(t *testing.T) {
	sc := launchSharedActionPlugin(t)
	m := sc.Manifest()
	if m.ID != "com.example.testaction" {
		t.Fatalf("manifest id = %q", m.ID)
	}
	want := map[ext.Kind]bool{ext.KindAction: true, ext.KindEventHook: true}
	if len(m.Kinds) != len(want) {
		t.Fatalf("manifest kinds = %v, want exactly %v", m.Kinds, want)
	}
	for _, k := range m.Kinds {
		if !want[k] {
			t.Fatalf("manifest kinds = %v, want exactly %v", m.Kinds, want)
		}
	}
}

// TestSharedClientDispensesTwoPrimaryKindsFromOneProcess is the core G2
// proof: Action and EventHook, dispensed off the SAME SharedClient, must be
// the SAME OS process (same PID) — and calling EventHook's OnEvent must be
// visible to a subsequent Action Invoke("__events") call, proving it is
// genuinely the same live impl instance underneath, not merely two
// processes that happen to share a PID by coincidence.
func TestSharedClientDispensesTwoPrimaryKindsFromOneProcess(t *testing.T) {
	sc := launchSharedActionPlugin(t)
	ctx := context.Background()

	action, err := sc.DispenseAction(ctx)
	if err != nil {
		t.Fatalf("DispenseAction: %v", err)
	}
	hook, err := sc.DispenseEventHook(ctx)
	if err != nil {
		t.Fatalf("DispenseEventHook: %v", err)
	}

	if action.Pid() == 0 || hook.Pid() == 0 {
		t.Fatalf("Pid()==0: action=%d hook=%d", action.Pid(), hook.Pid())
	}
	if action.Pid() != hook.Pid() {
		t.Fatalf("Action Pid=%d, EventHook Pid=%d, want the same process", action.Pid(), hook.Pid())
	}
	if action.Pid() != sc.Pid() {
		t.Fatalf("Action Pid=%d, SharedClient Pid=%d, want the same process", action.Pid(), sc.Pid())
	}

	if err := hook.OnEvent(ctx, ext.Event{Type: ext.EventFSMkdir, Path: "/probe"}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	res, err := action.Invoke(ctx, "", "__events", nil)
	if err != nil {
		t.Fatalf("Invoke(__events): %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("Invoke(__events) result type = %T, want map[string]any", res)
	}
	count, _ := m["count"].(float64) // JSON round trip decodes numbers as float64
	if count != 1 {
		t.Fatalf("recorded event count = %v, want 1 (proves Action and EventHook share one live impl instance)", m["count"])
	}
}

// TestSharedClientRepeatDispenseReturnsCachedClient pins the memoization:
// dispensing the same Kind twice off one SharedClient must return the
// identical wrapped client, not re-dispense (mirrors every non-shared
// pool's cache-hit behavior one layer down).
func TestSharedClientRepeatDispenseReturnsCachedClient(t *testing.T) {
	sc := launchSharedActionPlugin(t)
	ctx := context.Background()

	a1, err := sc.DispenseAction(ctx)
	if err != nil {
		t.Fatalf("DispenseAction 1: %v", err)
	}
	a2, err := sc.DispenseAction(ctx)
	if err != nil {
		t.Fatalf("DispenseAction 2: %v", err)
	}
	if a1 != a2 {
		t.Fatal("repeat DispenseAction must return the identical cached client")
	}
}

// TestSharedClientDispenseAssistSharesTheProcess pins that assist, too,
// rides the same shared process under G2 (unlike pluginproc's dedicated
// assistProcs pool for the non-shared path) — DispenseAssist must return a
// working client bound to the SAME PID as the primary Kinds.
func TestSharedClientDispenseAssistSharesTheProcess(t *testing.T) {
	sc := launchSharedActionPlugin(t)
	ctx := context.Background()

	assist, err := sc.DispenseAssist(ctx)
	if err != nil {
		t.Fatalf("DispenseAssist: %v", err)
	}
	if assist.Pid() != sc.Pid() {
		t.Fatalf("Assist Pid=%d, SharedClient Pid=%d, want the same process", assist.Pid(), sc.Pid())
	}
	res, err := assist.Assist(ctx, "", "probe", map[string]any{"q": "1"})
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok || m["form"] != "probe" {
		t.Fatalf("Assist result = %+v", res)
	}
}

// TestSharedClientDispenseUndeclaredKindFailsFastWithoutDispensing pins the
// manifest-based existence probe: actionplugin's manifest declares only
// Action and EventHook, never StorageDriver — DispenseStorageDriver must
// fail with ext.ErrCodeNotImplement, resolved purely from the already-loaded
// manifest (no wasted Dispense/RPC round trip against a service the peer
// never registered).
func TestSharedClientDispenseUndeclaredKindFailsFastWithoutDispensing(t *testing.T) {
	sc := launchSharedActionPlugin(t)
	ctx := context.Background()

	_, err := sc.DispenseStorageDriver(ctx)
	if err == nil {
		t.Fatal("DispenseStorageDriver against a manifest that never declares it must fail")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("DispenseStorageDriver error = %v (code %q, ok=%v), want ErrCodeNotImplement", err, code, ok)
	}
}

// TestSharedClientShutdownKillsEveryDispensedKind pins that Shutdown tears
// down the one shared process underneath every Kind dispensed off it —
// Action and EventHook's clients must both report Exited() afterward, since
// they share the identical *plugin.Client.
func TestSharedClientShutdownKillsEveryDispensedKind(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	ctx := context.Background()
	sc, err := host.LaunchShared(ctx, exec.Command(actionPluginBin), nil, ext.KindAction)
	if err != nil {
		t.Fatalf("launch shared action plugin: %v", err)
	}

	action, err := sc.DispenseAction(ctx)
	if err != nil {
		t.Fatalf("DispenseAction: %v", err)
	}
	hook, err := sc.DispenseEventHook(ctx)
	if err != nil {
		t.Fatalf("DispenseEventHook: %v", err)
	}

	if err := sc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !action.Exited() {
		t.Fatal("Action client must report Exited() after SharedClient.Shutdown")
	}
	if !hook.Exited() {
		t.Fatal("EventHook client must report Exited() after SharedClient.Shutdown")
	}
	if !sc.Exited() {
		t.Fatal("SharedClient must report Exited() after its own Shutdown")
	}
}

// buildLyingKindPluginBin compiles testdata/lyingkindplugin (Action only,
// but a manifest that also claims Kind=EventHook) to a temp path. Built
// on demand rather than wired into the shared TestMain fixture set (this
// package's other *PluginBin vars) since it exists solely for review round
// 1 item 2's regression test below.
func buildLyingKindPluginBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "lyingkindplugin")
	cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/lyingkindplugin")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skip("skip lyingkindplugin: " + fmt.Sprintf("build: %v\n%s", err, b))
	}
	if _, err := os.Stat(out); err != nil {
		t.Skip("skip lyingkindplugin: built binary missing: " + err.Error())
	}
	return out
}

// TestSharedClientDeclaredButUnservedKindMapsToNotImplement pins review
// round 1's item 2: lyingkindplugin's manifest declares Kind=EventHook (so
// hasKind's fast-path check passes and a real Dispense/RPC attempt is made),
// but the binary never actually implements ext.EventHook, so mountAll never
// registered EventHookService on its shared grpc.Server. Before this fix,
// the resulting codes.Unimplemented status propagated as a bare wrapped
// transport error; DispenseEventHook must now map it to
// ext.ErrCodeNotImplement, mirroring AssistClient.Assist's existing
// precedent for the same peer-shaped gap.
func TestSharedClientDeclaredButUnservedKindMapsToNotImplement(t *testing.T) {
	bin := buildLyingKindPluginBin(t)
	ctx := context.Background()
	sc, err := host.LaunchShared(ctx, exec.Command(bin), nil, ext.KindAction)
	if err != nil {
		t.Fatalf("launch shared lyingkind plugin: %v", err)
	}
	t.Cleanup(func() { _ = sc.Shutdown(context.Background()) })

	// Sanity: the manifest really does claim EventHook (otherwise this test
	// would only be exercising the OTHER fast-path — Kind not declared at
	// all — which TestSharedClientDispenseUndeclaredKindFailsFastWithoutDispensing
	// already covers).
	declared := false
	for _, k := range sc.Manifest().Kinds {
		if k == ext.KindEventHook {
			declared = true
		}
	}
	if !declared {
		t.Fatalf("test fixture manifest.Kinds = %v, want it to include EventHook (the lie this test needs)", sc.Manifest().Kinds)
	}

	_, err = sc.DispenseEventHook(ctx)
	if err == nil {
		t.Fatal("DispenseEventHook against a Kind the manifest declares but the binary never implements must fail")
	}
	code, ok := ext.CodeOf(err)
	if !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("DispenseEventHook error = %v (code %q, ok=%v), want ErrCodeNotImplement", err, code, ok)
	}

	// The Action Kind it DOES implement must still work normally — the
	// Unimplemented mapping is scoped to the one Kind that failed, not a
	// blanket poisoning of the whole SharedClient.
	action, err := sc.DispenseAction(ctx)
	if err != nil {
		t.Fatalf("DispenseAction (the Kind actually implemented): %v", err)
	}
	if _, err := action.Invoke(ctx, "", "echo", nil); err != nil {
		t.Fatalf("Invoke against the genuinely-implemented Kind: %v", err)
	}
}
