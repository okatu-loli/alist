package pluginproc

import (
	"context"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
)

// TestSharedProcessDefaultsOff pins the hard requirement: Manager.SharedProcess
// is false on a zero-value New() Manager, so every existing caller that never
// heard of G2 keeps getting the pre-G2 one-Kind-per-process behavior with no
// code change on its part.
func TestSharedProcessDefaultsOff(t *testing.T) {
	m := New()
	if m.SharedProcess {
		t.Fatal("SharedProcess must default to false")
	}
}

// TestSharedProcessOneProcessForTwoKinds is the core G2 proof at the Manager
// layer: with SharedProcess enabled, AcquireAction and AcquireEventHook for
// the SAME pluginID must be the SAME OS process (same PID) — actionplugin
// (testdata/actionplugin) implements both Kinds plus assist, and its main()
// now serves via ServeMulti (see main.go's G2 addition), so a real
// go-plugin subprocess is genuinely capable of this.
func TestSharedProcessOneProcessForTwoKinds(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.SharedProcess = true
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	action, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	defer relA()
	hookAny, relH, err := m.AcquireEventHook(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire event hook: %v", err)
	}
	defer relH()

	hcAction, ok := action.(*host.ActionClient)
	if !ok {
		t.Fatalf("action client is %T, want *host.ActionClient", action)
	}
	hcHook, ok := hookAny.(*host.EventHookClient)
	if !ok {
		t.Fatalf("event hook client is %T, want *host.EventHookClient", hookAny)
	}
	if hcAction.Pid() == 0 || hcHook.Pid() == 0 {
		t.Fatalf("Pid()==0: action=%d hook=%d", hcAction.Pid(), hcHook.Pid())
	}
	if hcAction.Pid() != hcHook.Pid() {
		t.Fatalf("Action Pid=%d, EventHook Pid=%d, want the same shared process", hcAction.Pid(), hcHook.Pid())
	}

	if m.ActionCount() != 1 {
		t.Fatalf("ActionCount=%d, want 1", m.ActionCount())
	}
	if m.EventHookCount() != 1 {
		t.Fatalf("EventHookCount=%d, want 1", m.EventHookCount())
	}
}

// TestSharedProcessInitRunsOnceAcrossKinds proves the two dispensed Kinds
// really are the same live impl instance, not merely two processes that
// happen to share a PID: an OnEvent call through the EventHook client must
// be visible to a subsequent Invoke("__events") call through the Action
// client (the fixture's escape hatch — see testdata/actionplugin/main.go).
// This is also indirect proof that initOnce (Phase G wave 1) did its job:
// two Kind dispenses sharing one process only ever got ONE Init call
// against ONE impl value.
func TestSharedProcessInitRunsOnceAcrossKinds(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.SharedProcess = true
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	action, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	defer relA()
	hook, relH, err := m.AcquireEventHook(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire event hook: %v", err)
	}
	defer relH()

	if err := hook.OnEvent(ctx, ext.Event{Type: ext.EventFSMkdir, Path: "/probe"}); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	res, err := action.Invoke(ctx, "", "__events", nil)
	if err != nil {
		t.Fatalf("Invoke(__events): %v", err)
	}
	m2, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("Invoke(__events) result type = %T", res)
	}
	count, _ := m2["count"].(float64)
	if count != 1 {
		t.Fatalf("recorded event count = %v, want 1", m2["count"])
	}
}

// TestSharedProcessEvictClearsTheWholeSharedEntry pins Evict's shared-mode
// contract: a single Evict call relaunches the ONE shared process backing
// pluginID, so every Kind dispensed off it (not just one) picks up fresh
// authority in one shot — mirrors TestEvictRelaunchesEveryKindPoolForPluginID
// (manager_test.go) one layer over, for the shared-process path.
func TestSharedProcessEvictClearsTheWholeSharedEntry(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.SharedProcess = true
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	action, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	defer relA()
	hook, relH, err := m.AcquireEventHook(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire event hook: %v", err)
	}
	defer relH()

	hcAction := action.(*host.ActionClient)
	hcHook := hook.(*host.EventHookClient)

	if err := m.Evict(ctx, pluginID, bin); err != nil {
		t.Fatalf("evict: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !hcAction.Exited() || !hcHook.Exited() {
		if time.Now().After(deadline) {
			t.Fatalf("evict must relaunch the whole shared entry: action exited=%v hook exited=%v", hcAction.Exited(), hcHook.Exited())
		}
		time.Sleep(10 * time.Millisecond)
	}

	action2, relA2, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action after evict: %v", err)
	}
	defer relA2()
	if action2.(*host.ActionClient).Pid() == hcAction.Pid() {
		t.Fatal("evict must relaunch a fresh process, not hand back the old PID")
	}
}

// TestSharedProcessRefsSharedAcrossKinds pins the aggregate-refcount
// requirement: releasing Action's reference must NOT tear down the shared
// process while EventHook's reference is still held, and the process must
// only actually go down once BOTH are released (neither Action nor
// EventHook is a lingering Kind, so the final release shuts it down
// synchronously rather than arming a linger timer).
func TestSharedProcessRefsSharedAcrossKinds(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.SharedProcess = true
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	action, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	hook, relH, err := m.AcquireEventHook(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire event hook: %v", err)
	}
	hcAction := action.(*host.ActionClient)
	hcHook := hook.(*host.EventHookClient)

	relA()
	if hcHook.Exited() {
		t.Fatal("releasing Action's ref must not kill the process while EventHook's ref is still held")
	}
	if err := hook.OnEvent(ctx, ext.Event{Type: ext.EventFSMkdir, Path: "/probe"}); err != nil {
		t.Fatalf("OnEvent against still-referenced shared process: %v", err)
	}
	// Count() reports "has this Kind been dispensed off the entry's current
	// generation," not "does this Kind specifically still hold a live
	// reference" — refs are aggregated across Kinds (sharedEntry.refs), not
	// tracked per Kind, so there is no narrower signal to report. The shared
	// entry is still alive (EventHook's ref keeps it up) and Action was
	// dispensed on it, so ActionCount staying at 1 here is the documented
	// semantics change (Manager.Count's doc comment), not a bug.
	if m.ActionCount() != 1 {
		t.Fatalf("ActionCount=%d, want 1 (still dispensed on the still-alive shared entry)", m.ActionCount())
	}
	if m.EventHookCount() != 1 {
		t.Fatalf("EventHookCount=%d, want 1 (still referenced)", m.EventHookCount())
	}

	relH()
	deadline := time.Now().Add(5 * time.Second)
	for !hcAction.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("releasing the LAST reference (EventHook's) must shut the shared process down")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.EventHookCount() != 0 {
		t.Fatalf("EventHookCount=%d, want 0 after releasing the last reference", m.EventHookCount())
	}
}

// TestSharedProcessAssistFallsBackWhenNoEntryYetThenSharesOnceOneExists pins
// AcquireAssist's split behavior under SharedProcess: with nothing yet
// acquired for pluginID, assist cannot bootstrap a shared entry on its own
// (no manifest RPC of its own) and falls back to the dedicated assistProcs
// pool; once some other Kind has stood up a shared entry, a LATER
// AcquireAssist call reuses it instead.
func TestSharedProcessAssistFallsBackWhenNoEntryYetThenSharesOnceOneExists(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.SharedProcess = true
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	assist1, relAssist1, err := m.AcquireAssist(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire assist (bootstrap fallback): %v", err)
	}
	defer relAssist1()
	hcAssist1 := assist1.(*host.AssistClient)
	if m.AssistCount() != 1 {
		t.Fatalf("AssistCount=%d, want 1 (dedicated fallback pool)", m.AssistCount())
	}

	action, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	defer relA()
	hcAction := action.(*host.ActionClient)
	if hcAction.Pid() == hcAssist1.Pid() {
		t.Fatal("assist bootstrap-fallback process must NOT be the same one Action just launched (they started independently)")
	}

	assist2, relAssist2, err := m.AcquireAssist(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire assist (shared reuse): %v", err)
	}
	defer relAssist2()
	hcAssist2 := assist2.(*host.AssistClient)
	if hcAssist2.Pid() != hcAction.Pid() {
		t.Fatalf("second AcquireAssist must share Action's process (Pid=%d), got Pid=%d", hcAction.Pid(), hcAssist2.Pid())
	}
}

// TestSharedProcessEvictTwiceInARowBothSucceed pins review round 1's item 1
// (CONFIRMED bug): evict used to reset sharedEntry.dispensed to an empty map
// on a successful relaunch without writing the just-used primaryKind back
// into it, so a SECOND Evict call with no Acquire in between (the real
// trigger: an admin saving a plugin's Grant twice in a row —
// server/handles/plugins.go's SetPluginGrant, or just a retried request)
// found firstRealKind(e.dispensed) empty and failed outright even though
// the shared entry was alive and perfectly evictable. Both calls here must
// succeed, and each must actually relaunch (fresh PID), not silently no-op.
//
// Review round 1, item 2 fixup: the original version of this test called
// AcquireAction BETWEEN the two Evicts as a "sanity check that the first
// evict really relaunched" — but acquireSharedKind's success path always
// writes the acquired Kind back into sharedEntry.dispensed (see
// acquireSharedKind, sharedpool.go), so that sanity call refilled the very
// map the bug drains empty. With it removed, the pre-fix code passed this
// test too (reverting the fix and re-running confirmed the second Evict
// erroring is what the test SHOULD have caught, and didn't). Verification
// now reads sharedEntry.client.Pid() directly (this test file is in
// `package pluginproc`, so it can) between and after each Evict — a
// read-only peek that never touches dispensed — instead of a real Acquire.
func TestSharedProcessEvictTwiceInARowBothSucceed(t *testing.T) {
	bin := buildActionPlugin(t)
	m := New()
	m.SharedProcess = true
	ctx := context.Background()
	const pluginID = "com.example.testaction"

	action, relA, err := m.AcquireAction(ctx, pluginID, bin)
	if err != nil {
		t.Fatalf("acquire action: %v", err)
	}
	defer relA()
	hc1 := action.(*host.ActionClient)

	if err := m.Evict(ctx, pluginID, bin); err != nil {
		t.Fatalf("first evict: %v", err)
	}
	// Peek the shared entry's live client directly — NOT via Acquire, which
	// would refill sharedEntry.dispensed and mask exactly the bug this test
	// exists to catch (see doc comment above).
	pidAfterFirst := m.shared.entries[pluginID].client.Pid()
	if pidAfterFirst == hc1.Pid() {
		t.Fatal("first evict must have relaunched a fresh process")
	}

	// The bug: this second Evict, with NOTHING acquired in between, used to
	// find sharedEntry.dispensed emptied out by the first evict and fail
	// with "shared entry has no dispensed primary Kind to relaunch from".
	if err := m.Evict(ctx, pluginID, bin); err != nil {
		t.Fatalf("second evict (back-to-back, no Acquire in between) must succeed: %v", err)
	}
	pidAfterSecond := m.shared.entries[pluginID].client.Pid()
	if pidAfterSecond == pidAfterFirst {
		t.Fatal("second evict must also have relaunched a fresh process, not silently no-op'd")
	}
}
