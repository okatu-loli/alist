package extension

import (
	"context"
	"strings"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
)

// F2 FileDecorator red team. The attacks target the "information leakage" and
// "撑爆响应 / DoS" surface of the fs/list field-group decoration protocol. The
// two headline worries from the brief:
//   - can a decorator learn/enumerate objects outside the user's view?  (No — the
//     host hands it only the caller-filtered refs; proven by
//     TestReg_DecorateFieldsPassesExactlyTheGivenRefs.)
//   - is the 200ms budget real isolation?  (It isolates LATENCY but NOT SIZE —
//     see FINDING A: a fast decorator returns an unbounded payload straight
//     through.)
//
// These drive the real DecorateFields path with stub decorators.

// hugeValueDecorator returns, for its group, ONE map entry per ref whose value
// is a valueLen-byte string. It returns immediately (well within budget), so it
// exercises the size dimension the time budget does not cover.
type hugeValueDecorator struct {
	group    string
	valueLen int
}

func (h *hugeValueDecorator) Manifest() ext.Manifest               { return ext.Manifest{} }
func (h *hugeValueDecorator) Init(context.Context, ext.Host) error { return nil }
func (h *hugeValueDecorator) Shutdown(context.Context) error       { return nil }
func (h *hugeValueDecorator) Groups() []string                     { return []string{h.group} }
func (h *hugeValueDecorator) Decorate(_ context.Context, _ string, refs []ext.ObjRef) (map[string]any, error) {
	big := strings.Repeat("A", h.valueLen)
	out := map[string]any{}
	for _, ref := range refs {
		out[ext.RefKey(ref)] = big
	}
	return out, nil
}

// FINDING A (MEDIUM, LIVE) — FileDecorator has no RESPONSE-SIZE budget.
//
// The per-group budget (DecorateFields / runGroup) bounds only WALL-CLOCK TIME:
// a decorator that returns FAST but with an arbitrarily large payload wins its
// channel receive before the deadline, so runGroup merges the whole thing and
// DecorateFields serializes it into the fs/list response. A malicious or buggy
// decorator (FileDecorator is not restricted to the native/trusted tier the way
// AccessProtocol is) can therefore amplify a single fs/list into a multi-MB (or
// GB) response and matching host-side allocation — "撑爆响应" — even though it
// "respected" the 200ms budget. The budget is not the isolation it appears to be
// for anything other than latency.
//
// This test characterizes the gap: an 8 MiB single field value flows through
// intact, in-budget, uncapped.
func TestReg_DecorateFieldsNoSizeBudget_FindingA(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(decoratorEntry("dec.huge")); err != nil {
		t.Fatal(err)
	}
	const bigLen = 8 << 20 // 8 MiB in a single field value
	RegisterFileDecoratorInstance("dec.huge", &hugeValueDecorator{group: "_big", valueLen: bigLen})
	t.Cleanup(func() { RegisterFileDecoratorInstance("dec.huge", nil) })

	refs := []ext.ObjRef{{Anchor: "/x"}}
	res := DecorateFields(context.Background(), r, []string{"_big"}, refs, 200*time.Millisecond)

	// FIXED (per-value byte cap): an 8 MiB value exceeds maxFieldValueBytes and is
	// dropped, so it does NOT flow through into the response.
	if v, ok := res.Fields["_big"]["/x"].(string); ok && len(v) == bigLen {
		t.Fatalf("REGRESSION: oversized %d-byte field value passed through uncapped", len(v))
	}
}

// keyInjectingDecorator returns field values for RefKeys it was NEVER given,
// including a path outside the requested page. It models a decorator that tries
// to smuggle extra rows into the response.
type keyInjectingDecorator struct{}

func (keyInjectingDecorator) Manifest() ext.Manifest               { return ext.Manifest{} }
func (keyInjectingDecorator) Init(context.Context, ext.Host) error { return nil }
func (keyInjectingDecorator) Shutdown(context.Context) error       { return nil }
func (keyInjectingDecorator) Groups() []string                     { return []string{"_labels"} }
func (keyInjectingDecorator) Decorate(_ context.Context, _ string, refs []ext.ObjRef) (map[string]any, error) {
	out := map[string]any{}
	// A legitimate value for the one requested ref...
	for _, ref := range refs {
		out[ext.RefKey(ref)] = "mine"
	}
	// ...plus injected keys for paths that were NOT in the batch.
	out["/dir/another-users-file.txt"] = "injected"
	out["/etc/shadow"] = "injected"
	return out, nil
}

// FINDING B (LOW / hardening) — DecorateFields does not filter the decorator's
// RETURNED keys against the refs it was asked about.
//
// runGroup blindly merges every key the decorator returns, and DecorateFields /
// FsList assign it straight to resp.Fields. So a decorator may inject field
// entries keyed to arbitrary paths (outside the current page, or paths the
// caller cannot even see). This is not a host confidentiality breach on its own
// (the injected VALUES originate in the plugin, which already has them), but it
// is an absent output-validation step: the host promises the client "fields for
// the page you listed" and instead returns whatever key space the decorator
// chooses. Combined with a frontend that renders `_labels` values by RefKey, an
// injected key becomes a response-injection primitive (and, if rendered as HTML,
// a stored-XSS vector — a frontend concern flagged for completeness).
//
// Suggested hardening: intersect the returned map with the RefKey set the host
// actually handed the decorator before merging.
func TestReg_DecorateFieldsReturnsUnfilteredKeys_FindingB(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(decoratorEntry("dec.inject")); err != nil {
		t.Fatal(err)
	}
	RegisterFileDecoratorInstance("dec.inject", keyInjectingDecorator{})
	t.Cleanup(func() { RegisterFileDecoratorInstance("dec.inject", nil) })

	refs := []ext.ObjRef{{Anchor: "/dir/mine.txt"}}
	res := DecorateFields(context.Background(), r, []string{"_labels"}, refs, 200*time.Millisecond)

	// FIXED (key intersection): the decorator's injected foreign keys are dropped;
	// only the requested ref's key survives.
	grp := res.Fields["_labels"]
	if grp == nil {
		t.Fatal("labels group missing (the one legitimate key should remain)")
	}
	if _, ok := grp["/dir/another-users-file.txt"]; ok {
		t.Fatal("REGRESSION: decorator-injected foreign key passed through")
	}
	if _, ok := grp["/etc/shadow"]; ok {
		t.Fatal("REGRESSION: second injected key passed through")
	}
	if _, ok := grp[ext.RefKey(refs[0])]; !ok {
		t.Fatal("the legitimate requested-ref key should remain")
	}
	if len(grp) != 1 {
		t.Fatalf("expected exactly the 1 requested key, got %d", len(grp))
	}
}

// recordingDecorator records exactly the refs it was handed, so a test can prove
// the host does not expand/add refs behind the caller's back.
type recordingDecorator struct {
	got []ext.ObjRef
}

func (d *recordingDecorator) Manifest() ext.Manifest               { return ext.Manifest{} }
func (d *recordingDecorator) Init(context.Context, ext.Host) error { return nil }
func (d *recordingDecorator) Shutdown(context.Context) error       { return nil }
func (d *recordingDecorator) Groups() []string                     { return []string{"_labels"} }
func (d *recordingDecorator) Decorate(_ context.Context, _ string, refs []ext.ObjRef) (map[string]any, error) {
	d.got = append(d.got, refs...)
	return nil, nil
}

// VERIFIED CORRECT (regression) — the host hands the decorator EXACTLY the refs
// it was given, nothing more. This is the DecorateFields half of the "a
// decorator can never probe objects outside the user's view" guarantee: the
// caller (FsList / FsFields) filters+authorizes the ref set, and DecorateFields
// forwards that set verbatim without widening it. (The authorization half — that
// FsList passes only op.FilterListing output and FsFields op.Authorize's every
// ref — lives in server/handles/fsread.go and is enforced there.)
func TestReg_DecorateFieldsPassesExactlyTheGivenRefs(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(decoratorEntry("dec.record")); err != nil {
		t.Fatal(err)
	}
	rec := &recordingDecorator{}
	RegisterFileDecoratorInstance("dec.record", rec)
	t.Cleanup(func() { RegisterFileDecoratorInstance("dec.record", nil) })

	refs := []ext.ObjRef{{Anchor: "/a"}, {Anchor: "/b"}}
	DecorateFields(context.Background(), r, []string{"_labels"}, refs, 200*time.Millisecond)

	if len(rec.got) != 2 || rec.got[0].Anchor != "/a" || rec.got[1].Anchor != "/b" {
		t.Fatalf("host must hand the decorator exactly the given refs, got %+v", rec.got)
	}
}

// VERIFIED CORRECT (regression) — a DISABLED decorator is never invoked, even if
// a client asks for the group it declares. decoratorsFor iterates
// Registry.ByKind, which returns only enabled entries, so a disabled decorator's
// group resolves to "no provider" (silently empty, not pending).
func TestReg_DecorateFieldsSkipsDisabledDecorator(t *testing.T) {
	r := NewRegistry()
	e := decoratorEntry("dec.off")
	e.enabled = false
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	// A live instance is registered, but the entry is disabled: it must not run.
	rec := &recordingDecorator{}
	RegisterFileDecoratorInstance("dec.off", rec)
	t.Cleanup(func() { RegisterFileDecoratorInstance("dec.off", nil) })

	res := DecorateFields(context.Background(), r, []string{"_labels"}, []ext.ObjRef{{Anchor: "/x"}}, 200*time.Millisecond)
	if len(rec.got) != 0 {
		t.Fatalf("disabled decorator must not be invoked, saw refs %+v", rec.got)
	}
	if len(res.Pending) != 0 || len(res.Fields) != 0 {
		t.Fatalf("disabled decorator group: want empty result, got %+v", res)
	}
}
