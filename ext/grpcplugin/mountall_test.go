package grpcplugin

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	pb "github.com/AlistGo/alist-ext/grpcproto"
)

// mountAllTestImpl implements ext.Action, ext.EventHook, and ext.Assist on
// the same value — a concrete, minimal stand-in for a G2 multi-Kind plugin
// (mirrors the shape testdata/actionplugin now takes, one module over,
// without needing to build a real subprocess for this map-level test).
type mountAllTestImpl struct{}

func (mountAllTestImpl) Manifest() ext.Manifest                     { return ext.Manifest{ID: "multi-kind-test"} }
func (mountAllTestImpl) Init(ctx context.Context, h ext.Host) error { return nil }
func (mountAllTestImpl) Shutdown(ctx context.Context) error         { return nil }

func (mountAllTestImpl) Invoke(ctx context.Context, principal ext.Principal, actionID string, args map[string]any) (any, error) {
	return nil, nil
}

func (mountAllTestImpl) OnEvent(ctx context.Context, e ext.Event) error { return nil }

func (mountAllTestImpl) Assist(ctx context.Context, principal ext.Principal, formID string, input map[string]any) (any, error) {
	return nil, nil
}

var (
	_ ext.Action    = mountAllTestImpl{}
	_ ext.EventHook = mountAllTestImpl{}
	_ ext.Assist    = mountAllTestImpl{}
)

func pluginMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMountAllMountsExactlyTheImplementedKinds pins mountAll's core
// contract: an impl implementing Action + EventHook + Assist gets exactly
// those three dispense names, nothing else — no Kind mountAll couldn't
// assert against gets mounted, and neither of the two implemented primary
// Kinds' own mountAssist call produces a duplicate/conflicting entry (map
// assignment collapses the redundant re-adds of pb.AssistPluginName from
// ActionPluginMap and EventHookPluginMap both auto-mounting it).
func TestMountAllMountsExactlyTheImplementedKinds(t *testing.T) {
	got := pluginMapKeys(mountAll(mountAllTestImpl{}))
	want := []string{pb.ActionPluginName, pb.AssistPluginName, pb.EventHookPluginName}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("mountAll keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mountAll keys = %v, want %v", got, want)
		}
	}
}

// storageDriverOnlyImpl implements only ext.StorageDriver (no Assist, no
// other Kind) — the common single-Kind case must still work through
// mountAll exactly like the dedicated PluginMap would.
type storageDriverOnlyImpl struct{}

func (storageDriverOnlyImpl) Manifest() ext.Manifest                     { return ext.Manifest{ID: "sd-only"} }
func (storageDriverOnlyImpl) Init(ctx context.Context, h ext.Host) error { return nil }
func (storageDriverOnlyImpl) Shutdown(ctx context.Context) error         { return nil }
func (storageDriverOnlyImpl) DriverConfig() ext.DriverConfig             { return ext.DriverConfig{} }
func (storageDriverOnlyImpl) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}
func (storageDriverOnlyImpl) DropInstance(ctx context.Context, storageID string) error { return nil }
func (storageDriverOnlyImpl) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	return nil, nil
}
func (storageDriverOnlyImpl) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	return nil, nil
}

var _ ext.StorageDriver = storageDriverOnlyImpl{}

func TestMountAllSingleKindImplGetsOnlyThatDispenseName(t *testing.T) {
	got := pluginMapKeys(mountAll(storageDriverOnlyImpl{}))
	if len(got) != 1 || got[0] != pb.PluginName {
		t.Fatalf("mountAll keys = %v, want [%s]", got, pb.PluginName)
	}
}

// TestMountAllAssistOnlyImplFallsBackToAssistAlone pins the defensive
// fallback: an impl that implements ext.Assist but no primary Kind at all
// (an unusual but not-forbidden shape) still gets mounted under
// pb.AssistPluginName, mirroring what a plugin author would get from
// calling ServeAssist(impl) directly for that shape.
type assistOnlyImpl struct{}

func (assistOnlyImpl) Assist(ctx context.Context, principal ext.Principal, formID string, input map[string]any) (any, error) {
	return nil, nil
}

var _ ext.Assist = assistOnlyImpl{}

func TestMountAllAssistOnlyImplFallsBackToAssistAlone(t *testing.T) {
	got := pluginMapKeys(mountAll(assistOnlyImpl{}))
	if len(got) != 1 || got[0] != pb.AssistPluginName {
		t.Fatalf("mountAll keys = %v, want [%s]", got, pb.AssistPluginName)
	}
}

// TestMountAllWithKindsNarrowsDispenseSet pins the WithKinds escape hatch:
// an impl satisfying two primary Kinds can be restricted to mounting only
// one of them. Assist is never gated by WithKinds — it rides along with
// whichever primary Kind(s) survive the filter, exactly like an unfiltered
// mountAll call, since every XxxPluginMap auto-mounts it independently of
// the WithKinds list.
func TestMountAllWithKindsNarrowsDispenseSet(t *testing.T) {
	got := pluginMapKeys(mountAll(mountAllTestImpl{}, WithKinds(ext.KindAction)))
	want := []string{pb.ActionPluginName, pb.AssistPluginName}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("mountAll(WithKinds(Action)) keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mountAll(WithKinds(Action)) keys = %v, want %v", got, want)
		}
	}
}

// TestMountAllWithKindsExcludingEverySatisfiedKindStillFallsBackToAssist
// pins a corner of the WithKinds + assist-fallback interaction: if the
// allow-list excludes every Kind impl actually satisfies, mountAll ends up
// with an empty primary-Kind set — the same "nothing mounted yet" state as
// an impl satisfying no Kind at all — so the assist-alone fallback kicks in
// for an impl that also implements ext.Assist, rather than serving nothing.
func TestMountAllWithKindsExcludingEverySatisfiedKindStillFallsBackToAssist(t *testing.T) {
	got := pluginMapKeys(mountAll(mountAllTestImpl{}, WithKinds(ext.KindAPIRoute)))
	if len(got) != 1 || got[0] != pb.AssistPluginName {
		t.Fatalf("mountAll(WithKinds(APIRoute)) keys = %v, want [%s] (assist-alone fallback)", got, pb.AssistPluginName)
	}
}

// TestAllPluginMapCoversAllFifteenDispenseNames pins the client-side stub
// set host.LaunchShared relies on: every one of the 15 dispense names must
// be present so Dispense(name) never fails locally regardless of which
// Kinds the actual subprocess implements.
func TestAllPluginMapCoversAllFifteenDispenseNames(t *testing.T) {
	got := pluginMapKeys(AllPluginMap())
	want := []string{
		pb.PluginName, pb.AuthPluginName, pb.AuthNPluginName, pb.BgServicePluginName,
		pb.APIRoutePluginName, pb.ActionPluginName, pb.EventHookPluginName,
		pb.ScheduledJobPluginName, pb.NotificationPluginName, pb.OfflineDownloadToolPluginName,
		pb.SearchIndexerPluginName, pb.ArchiveToolPluginName, pb.FileDecoratorPluginName,
		pb.AccessProtocolPluginName, pb.AssistPluginName,
	}
	sort.Strings(want)
	if len(got) != 15 {
		t.Fatalf("AllPluginMap has %d keys, want 15: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllPluginMap keys = %v, want %v", got, want)
		}
	}
}

// TestHostAllPluginMapCoversAllFifteenDispenseNames mirrors the above for
// the reverse-host-bridge variant.
func TestHostAllPluginMapCoversAllFifteenDispenseNames(t *testing.T) {
	got := pluginMapKeys(HostAllPluginMap(hostServerFactory))
	if len(got) != 15 {
		t.Fatalf("HostAllPluginMap has %d keys, want 15: %v", len(got), got)
	}
}

// bgAndScheduledJobImpl implements both ext.BackgroundService and
// ext.ScheduledJob on the same value — the one pair mountAll's doc comment
// calls out as signature-identical-but-contract-incompatible on Run(ctx)
// error (review round 1, item 3).
type bgAndScheduledJobImpl struct{}

func (bgAndScheduledJobImpl) Manifest() ext.Manifest                     { return ext.Manifest{ID: "bg-and-schedjob"} }
func (bgAndScheduledJobImpl) Init(ctx context.Context, h ext.Host) error { return nil }
func (bgAndScheduledJobImpl) Shutdown(ctx context.Context) error         { return nil }
func (bgAndScheduledJobImpl) Run(ctx context.Context) error              { return nil }
func (bgAndScheduledJobImpl) Status(ctx context.Context) (ext.ServiceStatus, error) {
	return ext.ServiceStatus{State: "running"}, nil
}
func (bgAndScheduledJobImpl) Schedule() string { return "@hourly" }

var (
	_ ext.BackgroundService = bgAndScheduledJobImpl{}
	_ ext.ScheduledJob      = bgAndScheduledJobImpl{}
)

// TestMountAllBackgroundServiceScheduledJobClashMountsOnlyOne pins the fix:
// an impl satisfying both BackgroundService and ScheduledJob must NOT get
// both dispense names mounted (whichever RPC arrives first would run Run
// against a contract the other Kind never agreed to) — mountAll keeps
// BackgroundService and drops ScheduledJob, without erroring.
func TestMountAllBackgroundServiceScheduledJobClashMountsOnlyOne(t *testing.T) {
	got := pluginMapKeys(mountAll(bgAndScheduledJobImpl{}))
	if len(got) != 1 || got[0] != pb.BgServicePluginName {
		t.Fatalf("mountAll keys = %v, want exactly [%s] (BackgroundService kept, ScheduledJob dropped)", got, pb.BgServicePluginName)
	}
}

// TestMountAllWithKindsCanChooseScheduledJobOverBackgroundService pins the
// documented override: explicitly naming only ext.KindScheduledJob in
// WithKinds (never mentioning ext.KindBackgroundService) selects the other
// side of the clash instead of the default tie-break.
func TestMountAllWithKindsCanChooseScheduledJobOverBackgroundService(t *testing.T) {
	got := pluginMapKeys(mountAll(bgAndScheduledJobImpl{}, WithKinds(ext.KindScheduledJob)))
	if len(got) != 1 || got[0] != pb.ScheduledJobPluginName {
		t.Fatalf("mountAll(WithKinds(ScheduledJob)) keys = %v, want exactly [%s]", got, pb.ScheduledJobPluginName)
	}
}
