package extension

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// stubBundle is a composite that implements FeatureBundle + APIRoute + EventHook:
// the share/labels底座 shape. It records event deliveries so the test can prove
// both contributed points fire after expansion.
type stubBundle struct {
	routes     []ext.RouteDecl
	eventsSeen []ext.EventType
}

func (s *stubBundle) Manifest() ext.Manifest               { return ext.Manifest{} }
func (s *stubBundle) Init(context.Context, ext.Host) error { return nil }
func (s *stubBundle) Shutdown(context.Context) error       { return nil }
func (s *stubBundle) Bundle() ext.BundleSpec {
	return ext.BundleSpec{NeedsStore: true, NeedsSign: true}
}

// APIRoute leg.
func (s *stubBundle) Routes() []ext.RouteDecl { return s.routes }
func (s *stubBundle) Handle(_ context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{Status: 200, Body: []byte("bundle:" + req.URL)}, nil
}

// EventHook leg.
func (s *stubBundle) OnEvent(_ context.Context, e ext.Event) error {
	s.eventsSeen = append(s.eventsSeen, e.Type)
	return nil
}

func bundleEntry(id string, apis ...string) *Entry {
	return &Entry{
		Manifest: ext.Manifest{ID: id, Kinds: []ext.Kind{ext.KindFeatureBundle},
			Permissions: ext.Permissions{HostAPIs: apis}},
		Kind:    ext.KindFeatureBundle,
		enabled: true,
	}
}

// The core F2c property: after ExpandFeatureBundle, a bundle declaring 1 APIRoute
// + 1 EventHook has BOTH reachable through the reused F1 wiring points, even
// though its primary Kind is FeatureBundle.
func TestExpandFeatureBundleWiresAPIRouteAndEventHook(t *testing.T) {
	r := NewRegistry()
	// The bundle is granted "events" so EventHook delivery is not gated out.
	if err := r.Register(bundleEntry("bundle.share", CapEvents)); err != nil {
		t.Fatal(err)
	}
	bundle := &stubBundle{routes: []ext.RouteDecl{{Prefix: "/manage"}}}
	RegisterFeatureBundleInstance("bundle.share", bundle)
	t.Cleanup(func() { CollapseFeatureBundle("bundle.share") })

	exp, err := ExpandFeatureBundle(r, "bundle.share")
	if err != nil {
		t.Fatalf("ExpandFeatureBundle: %v", err)
	}
	if !exp.Spec.NeedsStore || !exp.Spec.NeedsSign {
		t.Fatalf("bundle spec not surfaced: %+v", exp.Spec)
	}
	if len(exp.Kinds) != 2 {
		t.Fatalf("want APIRoute+EventHook expanded, got %v", exp.Kinds)
	}

	// APIRoute leg: reachable through DispatchAPIRoute despite primary Kind=Bundle.
	resp, err := DispatchAPIRoute(context.Background(), r, "bundle.share", "/manage/x", false,
		&ext.HTTPRequest{Method: "GET", URL: "/manage/x"})
	if err != nil {
		t.Fatalf("DispatchAPIRoute to bundle: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "bundle:/manage/x" {
		t.Fatalf("unexpected route resp: %+v", resp)
	}

	// EventHook leg: DispatchEvent fans out and the bundle's OnEvent fires.
	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSMove, Path: "/a", DstPath: "/b"})
	if len(bundle.eventsSeen) != 1 || bundle.eventsSeen[0] != ext.EventFSMove {
		t.Fatalf("bundle EventHook did not fire: %v", bundle.eventsSeen)
	}
}

// A bundle whose EventHook leg is NOT granted "events" is still reachable as an
// APIRoute but its OnEvent is gated out (the dispatch grant gate is unchanged by
// expansion).
func TestExpandedBundleEventHookStillGrantGated(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(bundleEntry("bundle.nogrant")); err != nil { // no CapEvents
		t.Fatal(err)
	}
	bundle := &stubBundle{routes: []ext.RouteDecl{{Prefix: "/m"}}}
	RegisterFeatureBundleInstance("bundle.nogrant", bundle)
	t.Cleanup(func() { CollapseFeatureBundle("bundle.nogrant") })

	if _, err := ExpandFeatureBundle(r, "bundle.nogrant"); err != nil {
		t.Fatal(err)
	}
	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSRemove, Path: "/a"})
	if len(bundle.eventsSeen) != 0 {
		t.Fatalf("ungranted bundle EventHook must be skipped, saw %v", bundle.eventsSeen)
	}
}

// TestExpandFeatureBundleMultiKindManifestUsesManifestTruthSource mirrors
// TestInvokeActionMultiKindManifestUsesManifestTruthSource: an entry
// registered under a different primary Kind (Kind=EventHook) whose Manifest
// additionally declares FeatureBundle (Kinds: [EventHook, FeatureBundle])
// never gets an extraKinds(FeatureBundle) entry — nothing ever
// AddKindIndex's KindFeatureBundle onto an entry, the expansion only ever
// adds OTHER kinds onto a bundle — so e.HasKind(FeatureBundle) alone would
// wrongly refuse to expand it. DeclaresKind must let it through.
func TestExpandFeatureBundleMultiKindManifestUsesManifestTruthSource(t *testing.T) {
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{ID: "bundle.multikind", Kinds: []ext.Kind{ext.KindEventHook, ext.KindFeatureBundle}},
		Kind:     ext.KindEventHook, // registered/indexed under its primary Kind
		enabled:  true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	bundle := &stubBundle{routes: []ext.RouteDecl{{Prefix: "/m"}}}
	RegisterFeatureBundleInstance("bundle.multikind", bundle)
	t.Cleanup(func() { CollapseFeatureBundle("bundle.multikind") })

	exp, err := ExpandFeatureBundle(r, "bundle.multikind")
	if err != nil {
		t.Fatalf("ExpandFeatureBundle on multi-Kind manifest: %v", err)
	}
	if len(exp.Kinds) != 2 {
		t.Fatalf("want APIRoute+EventHook expanded, got %v", exp.Kinds)
	}
}

func TestExpandFeatureBundleUnknownOrDisabled(t *testing.T) {
	r := NewRegistry()
	if _, err := ExpandFeatureBundle(r, "nope"); err == nil {
		t.Fatal("unknown bundle: want error, got nil")
	}
	e := bundleEntry("bundle.off")
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandFeatureBundle(r, "bundle.off"); err == nil {
		t.Fatal("disabled bundle: want error, got nil")
	}
}

// stubGRPCEventHook is a minimal ext.EventHook double installed through
// SetEventHookFactory (the gRPC dispatch seam), standing in for a real
// subprocess client so the test can prove DispatchEvent actually reaches the
// factory rather than an in-process instance.
type stubGRPCEventHook struct {
	calls *int
}

func (h stubGRPCEventHook) Manifest() ext.Manifest               { return ext.Manifest{} }
func (h stubGRPCEventHook) Init(context.Context, ext.Host) error { return nil }
func (h stubGRPCEventHook) Shutdown(context.Context) error       { return nil }
func (h stubGRPCEventHook) OnEvent(_ context.Context, _ ext.Event) error {
	*h.calls++
	return nil
}

// TestExpandFeatureBundleGRPCEntryIndexesDeclaredSubKinds is the F2c property
// for a DISK (gRPC) FeatureBundle: since there is no single Go value to
// resolve and interface-assert across the process boundary, expansion must
// fall back to the manifest's OWN declared Kinds list, index each expandable
// sub-Kind (here EventHook + ScheduledJob) onto the registry, and register NO
// in-process instance — dispatch must still reach a live handler, but only
// through that Kind's own gRPC factory seam (SetEventHookFactory here),
// exactly like a plain (non-bundle) gRPC plugin declaring the Kind directly.
func TestExpandFeatureBundleGRPCEntryIndexesDeclaredSubKinds(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{
		ID:    "bundle.grpc",
		Kinds: []ext.Kind{ext.KindFeatureBundle, ext.KindEventHook, ext.KindScheduledJob},
		Permissions: ext.Permissions{
			HostAPIs: []string{CapEvents},
		},
	}
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err != nil {
		t.Fatalf("RegisterPluginMode: %v", err)
	}

	exp, err := ExpandFeatureBundle(r, "bundle.grpc")
	if err != nil {
		t.Fatalf("ExpandFeatureBundle: %v", err)
	}
	if len(exp.Kinds) != 2 {
		t.Fatalf("want EventHook+ScheduledJob expanded, got %v", exp.Kinds)
	}
	if exp.Spec != (ext.BundleSpec{}) {
		t.Fatalf("gRPC expansion must leave BundleSpec zero (in-process-only wiring aid), got %+v", exp.Spec)
	}

	entry, ok := r.Get("bundle.grpc")
	if !ok || !entry.HasKind(ext.KindEventHook) || !entry.HasKind(ext.KindScheduledJob) {
		t.Fatalf("entry not indexed under contributed kinds: %+v", entry)
	}

	// No in-process instance was registered for the bundle id.
	if h, _ := resolveEventHook(entry); h != nil {
		t.Fatalf("gRPC bundle expansion must not register an in-process EventHook instance")
	}

	// Dispatch must still reach a live handler, through the gRPC factory seam.
	var calls int
	SetEventHookFactory(func(e *Entry) (ext.EventHook, func(), error) {
		if e.Manifest.ID != "bundle.grpc" {
			t.Fatalf("factory called for unexpected id %q", e.Manifest.ID)
		}
		return stubGRPCEventHook{calls: &calls}, func() {}, nil
	})
	t.Cleanup(func() { SetEventHookFactory(nil) })

	DispatchEvent(context.Background(), r, ext.Event{Type: ext.EventFSMove})
	if calls != 1 {
		t.Fatalf("DispatchEvent via gRPC factory: calls = %d, want 1", calls)
	}
}

// TestExpandFeatureBundleGRPCEntrySkipsNonExpandableKind covers a manifest
// that mixes an unrelated Kind (StorageDriver) in with FeatureBundle+EventHook:
// the non-expandable Kind must be skipped (logged, not fatal) while the
// legitimate EventHook leg still expands.
func TestExpandFeatureBundleGRPCEntrySkipsNonExpandableKind(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{
		ID:    "bundle.grpc.mixed",
		Kinds: []ext.Kind{ext.KindFeatureBundle, ext.KindEventHook, ext.KindStorageDriver},
	}
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err != nil {
		t.Fatalf("RegisterPluginMode: %v", err)
	}
	exp, err := ExpandFeatureBundle(r, "bundle.grpc.mixed")
	if err != nil {
		t.Fatalf("ExpandFeatureBundle: %v", err)
	}
	if len(exp.Kinds) != 1 || exp.Kinds[0] != ext.KindEventHook {
		t.Fatalf("want only EventHook expanded, got %v", exp.Kinds)
	}
	entry, _ := r.Get("bundle.grpc.mixed")
	if entry.HasKind(ext.KindStorageDriver) {
		t.Fatal("StorageDriver must not be indexed by feature bundle expansion")
	}
}

// TestFeatureBundleGRPCReexpandAfterRestart simulates a server restart: a
// gRPC-mode FeatureBundle entry is expanded, then re-registered fresh (as
// registerActive/RegisterPluginMode does on every Enable/LoadEnabled call —
// Deregister wipes the prior extraKinds), and expanding it again must restore
// the same reachability. This is the registry-level half of the Manager
// restart property; TestManagerLoadEnabledReexpandsFeatureBundle in
// lifecycle_test.go covers the full Manager path.
func TestFeatureBundleGRPCReexpandAfterRestart(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{
		ID:    "bundle.restart",
		Kinds: []ext.Kind{ext.KindFeatureBundle, ext.KindEventHook},
	}
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpandFeatureBundle(r, "bundle.restart"); err != nil {
		t.Fatal(err)
	}
	entry, _ := r.Get("bundle.restart")
	if !entry.HasKind(ext.KindEventHook) {
		t.Fatal("expected EventHook indexed before restart")
	}

	// "Restart": re-register (fresh Entry, extraKinds reset).
	r.Deregister("bundle.restart")
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err != nil {
		t.Fatal(err)
	}
	entry, _ = r.Get("bundle.restart")
	if entry.HasKind(ext.KindEventHook) {
		t.Fatal("fresh registration must not carry over the prior expansion")
	}

	if _, err := ExpandFeatureBundle(r, "bundle.restart"); err != nil {
		t.Fatalf("re-expand after restart: %v", err)
	}
	entry, _ = r.Get("bundle.restart")
	if !entry.HasKind(ext.KindEventHook) {
		t.Fatal("re-expansion after restart must restore reachability")
	}
}
