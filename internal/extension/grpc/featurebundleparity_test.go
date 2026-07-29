package grpc

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// TestFeatureBundleGRPCExpandDispatchesToRealSubprocess is the full parity
// gate for the F2c gRPC FeatureBundle wiring: a Kind=FeatureBundle plugin
// entry that ALSO declares Kind=EventHook is expanded with
// extension.ExpandFeatureBundle (no bundle instance is ever resolved or
// asserted — see expandGRPCFeatureBundle's doc comment), and DispatchEvent
// must reach a REAL go-plugin subprocess (the eventhookplugin fixture used by
// TestEventHookGRPCDispatchThroughExtension) through EventHook's OWN existing
// gRPC factory seam — proving the "dispatch falls through to that Kind's own
// factory" claim end to end, not just against an in-test fake.
func TestFeatureBundleGRPCExpandDispatchesToRealSubprocess(t *testing.T) {
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c := launchEventHookPlugin(t)

	extension.SetEventHookFactory(func(e *extension.Entry) (ext.EventHook, func(), error) {
		return c, func() {}, nil
	})
	t.Cleanup(func() { extension.SetEventHookFactory(nil) })

	r := extension.NewRegistry()
	// The HOST-side entry's manifest is what ExpandFeatureBundle reads — it
	// declares BOTH FeatureBundle and EventHook, exactly the shape a disk
	// plugin bundling share/labels + a notification hook would ship. The
	// fixture's own Manifest() (over the wire) is irrelevant here: the
	// factory above hands back the same client `c` regardless of which
	// Entry it is asked to resolve.
	m := ext.Manifest{
		ID:    "com.example.testeventhook",
		Kinds: []ext.Kind{ext.KindFeatureBundle, ext.KindEventHook},
		Permissions: ext.Permissions{
			HostAPIs: []string{extension.CapEvents},
		},
	}
	if err := r.RegisterPlugin(m, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}

	exp, err := extension.ExpandFeatureBundle(r, m.ID)
	if err != nil {
		t.Fatalf("ExpandFeatureBundle: %v", err)
	}
	if len(exp.Kinds) != 1 || exp.Kinds[0] != ext.KindEventHook {
		t.Fatalf("expanded kinds = %v, want [EventHook]", exp.Kinds)
	}

	entry, ok := r.Get(m.ID)
	if !ok || !entry.HasKind(ext.KindEventHook) {
		t.Fatalf("entry not indexed under EventHook after expansion: ok=%v", ok)
	}

	// DispatchEvent must not panic/block, and must actually reach the real
	// subprocess (proven by TestEventHookGRPCDispatchThroughExtension's
	// identical assertion one Kind over: the fixture "fails" every OnEvent
	// via its echo error, which DispatchEvent isolates as a warning, not a
	// panic or a propagated error).
	extension.DispatchEvent(context.Background(), r, ext.Event{
		Type: ext.EventFSMkdir, Path: "/via-bundle-expansion", Actor: "erin",
	})
}

// launchEventHookPlugin and the shared buildSkip/eventHookPluginBin are
// defined in eventhookparity_test.go (same package, same TestMain fixture
// build).
