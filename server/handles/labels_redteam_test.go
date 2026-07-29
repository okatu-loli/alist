package handles

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// Labels red-team regressions (design §6). These are adversarial: each asserts
// an attack is REFUSED. They complement labels_test.go by hammering the two
// highest-value surfaces the design calls out — (1) applying/removing labels on
// paths the caller has no write on, spelled every way a path can be spelled
// (absolute, backslash, dot-dot, doubled separators) to prove apply's path and
// Authorize's path are the same resolved path; and (2) the #RRGGBB color guard,
// the single thing keeping a stored value out of a frontend style attribute.

// ---------------------------------------------------------------------------
// Attack 4 (color CSS injection): the op-layer color guard is the choke point
// for every write path (create + update). If any of these values were stored
// they would land verbatim in a `style="background:<color>"`, so every one must
// be rejected and nothing must be persisted.
// ---------------------------------------------------------------------------

// evilColors is the CSS-injection battery: multi-value declarations, url()/
// expression(), 3/8-digit hex, non-hex, and — the subtle ones — leading/
// trailing/embedded whitespace and newlines that a non-anchored or multiline
// regex would let through.
var evilColors = []string{
	`red;background:url(javascript:alert(1))`,
	`#ffffff;background:red`,
	`#fff`,      // 3-digit shorthand (not #RRGGBB)
	`#ffffffff`, // 8-digit (RRGGBBAA)
	`#gggggg`,   // non-hex letters
	`rgb(255,0,0)`,
	`expression(alert(1))`,
	`url(evil)`,
	`#ffffff `,                            // trailing space
	` #ffffff`,                            // leading space
	`#ffffff;`,                            // trailing semicolon
	`#ffffff/**/`,                         // CSS comment tail
	"#ffffff\n",                           // trailing newline ($ must be \z, not \Z)
	"#ffffff\ncolor:red",                  // newline-smuggled second declaration
	"#ffffff\n</style><script>x</script>", // newline then markup break-out
	"#ffff\tff",                           // embedded tab
	"#ff ffff",                            // embedded space
	`#fffff`,                              // 5 digits
	`javascript:alert(1)`,
	`#ffffff"onload="alert(1)`, // attribute break-out attempt
	`＃ffffff`,                  // fullwidth hash homoglyph
}

// TestLabelColorInjectionRejected drives every evilColors value through the op
// create + update guards and asserts (a) an error is returned and (b) nothing
// was persisted / a prior valid color is left intact. This is the design §6
// "color/name 校验防注入" contract as a permanent regression.
func TestLabelColorInjectionRejected(t *testing.T) {
	for i, color := range evilColors {
		color := color
		t.Run(fmt.Sprintf("create-%d", i), func(t *testing.T) {
			name := fmt.Sprintf("rt-color-create-%d", i)
			l := &model.Label{Name: name, Color: color}
			if err := op.CreateLabel(l); err == nil {
				t.Fatalf("CreateLabel accepted injectable color %q (would reach a style attr)", color)
			}
			// Nothing must have been persisted under that name.
			if got, err := op.ListLabels(); err == nil {
				for _, x := range got {
					if x.Name == name {
						t.Fatalf("rejected color %q still created a row (color=%q)", color, x.Color)
					}
				}
			}
		})
	}

	// Update must guard the same way: a valid label's color cannot be mutated
	// into an injectable value, and a rejected update must leave the stored
	// color untouched.
	for i, color := range evilColors {
		color := color
		t.Run(fmt.Sprintf("update-%d", i), func(t *testing.T) {
			base := &model.Label{Name: fmt.Sprintf("rt-color-update-%d", i), Color: "#0a0b0c"}
			if err := op.CreateLabel(base); err != nil {
				t.Fatalf("seed: %v", err)
			}
			t.Cleanup(func() { _ = op.DeleteLabel(base.ID) })

			base.Color = color
			if err := op.UpdateLabel(base); err == nil {
				t.Fatalf("UpdateLabel accepted injectable color %q", color)
			}
			reloaded, err := op.GetLabel(base.ID)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Color != "#0a0b0c" {
				t.Fatalf("rejected update mutated stored color to %q", reloaded.Color)
			}
		})
	}
}

// TestLabelColorValidAccepted is the negative control: legitimate #RRGGBB values
// (both cases) still pass, so the guard above is rejecting on the injection
// shape and not just refusing everything.
func TestLabelColorValidAccepted(t *testing.T) {
	for i, color := range []string{"#ffffff", "#000000", "#AbCdEf", "#0A0B0C", ""} {
		l := &model.Label{Name: fmt.Sprintf("rt-color-ok-%d", i), Color: color}
		if err := op.CreateLabel(l); err != nil {
			t.Fatalf("valid color %q rejected: %v", color, err)
		}
		t.Cleanup(func() { _ = op.DeleteLabel(l.ID) })
	}
}

// ---------------------------------------------------------------------------
// Attack 4b (name): the design deliberately stores the name as plain text and
// pushes HTML-escaping to the frontend renderer. This test PINS that contract:
// the backend stores the name verbatim (no silent stripping/encoding that a
// consumer might wrongly assume made it HTML-safe). It is a documentation
// regression — if a future change starts trusting the backend to sanitize, the
// assumption is now explicit.
// ---------------------------------------------------------------------------

func TestLabelNameStoredVerbatim(t *testing.T) {
	payload := `<script>alert(1)</script>`
	l := &model.Label{Name: payload, Color: "#123456"}
	if err := op.CreateLabel(l); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = op.DeleteLabel(l.ID) })
	got, err := op.GetLabel(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Backend does NOT sanitize; it stores raw. (Only surrounding whitespace is
	// trimmed by normalizeLabelName.) Frontend MUST escape on render.
	if got.Name != payload {
		t.Fatalf("name not stored verbatim: got %q want %q — if this changed, re-audit the render-side escaping assumption", got.Name, payload)
	}
}

// ---------------------------------------------------------------------------
// Attack 1 (unauthorized relabel + path traversal): the fixture user has write
// ONLY on <mount>/vis. Every spelling of a path that resolves to <mount>/sec
// (or anywhere else it lacks write) must be denied by the ActWrite gate, and a
// denied apply must never create the binding. This proves the handler's
// Authorize path and op.ApplyLabel's anchor path are the same resolved path —
// there is no spelling that authorizes /vis but writes /sec.
// ---------------------------------------------------------------------------

func TestLabelApplyPathTraversalDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Spellings that all aim at the unreadable/unwritable sec subtree, plus a
	// couple of raw-relative escapes. Each must end at 403: JoinPath now only
	// FixAndCleanPath-normalizes the request (".." is folded, capped at "/"),
	// then op.Authorize denies the resolved path. Must leave sec/s.txt unlabeled.
	spellings := []string{
		`%s/sec/s.txt`,        // absolute direct
		`..\sec\s.txt`,        // backslash dot-dot (normalized, then Authorize denies)
		`../sec/s.txt`,        // unix dot-dot (clean-collapsed, then Authorize denies)
		`%s/vis/../sec/s.txt`, // dot-dot back out of vis (Clean folds; resolved path denied)
		`\%s\sec\s.txt`,       // all-backslash absolute
		`%s/./sec/s.txt`,      // dot segment
		`%s//sec//s.txt`,      // doubled separators
		`%s/sec/../sec/s.txt`, // dot-dot round trip
	}

	for i, spell := range spellings {
		spell := spell
		t.Run(fmt.Sprintf("spelling-%d", i), func(t *testing.T) {
			u, red, mount := setupLabelFixture(t)
			var reqPath string
			if strings.Contains(spell, "%s") {
				reqPath = fmt.Sprintf(spell, mount)
			} else {
				reqPath = spell
			}
			w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q,"label_id":%d}`, reqPath, red.ID), LabelApply)
			if got := respCode(t, w); got != 403 {
				t.Fatalf("apply via %q must be denied (403), got %d: %s", reqPath, got, w.Body.String())
			}
			// The canonical target must remain unlabeled regardless of spelling.
			labels, err := op.LabelsOf(mount + "/sec/s.txt")
			if err != nil {
				t.Fatal(err)
			}
			if len(labels) != 0 {
				t.Fatalf("denied apply via %q still bound a label to sec/s.txt: %v", reqPath, labels)
			}
		})
	}
}

// TestLabelApplyDeniedCreatesNoNode is the "information-disclosure via node
// anchoring" check: a denied apply to a never-anchored path under an unwritable
// subtree must NOT leave a node behind (which would betray that the path was
// probed / let a later reader see it). Because Authorize runs before
// EnsureNodeChain, no node should be created.
func TestLabelApplyDeniedCreatesNoNode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, red, mount := setupLabelFixture(t)
	// A brand-new path under the unwritable sec subtree.
	target := mount + "/sec/probe-does-not-exist.txt"
	w := driveJSON(u, "POST", fmt.Sprintf(`{"path":%q,"label_id":%d}`, target, red.ID), LabelApply)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("apply to unwritable path must be 403, got %d: %s", got, w.Body.String())
	}
	// LabelsOf resolves the node WITHOUT creating one; an empty (nil-error)
	// result means no node/binding was fabricated by the denied apply.
	labels, err := op.LabelsOf(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Fatalf("denied apply fabricated a binding at a probe path: %v", labels)
	}
}

// ---------------------------------------------------------------------------
// Attack 2 (filter-view leak beyond raw scope): LabelFiles must run each bound
// node through the FULL Authorize(ActRead) — not just the coarse role scope —
// so a hide-meta'd path inside the caller's own readable subtree is still
// dropped from the "files with this label" view. Binds the same label to a
// visible file and a hidden-by-meta file, both under /vis (both role-readable),
// and asserts only the non-hidden one is listed.
// ---------------------------------------------------------------------------

func TestLabelFilesHonorsHideMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, red, mount := setupLabelFixture(t)

	// Hide anything matching "hidden.*" under the (readable+writable) vis subtree.
	if err := op.CreateMeta(&model.Meta{Path: mount + "/vis", Hide: "hidden.*", HSub: true}); err != nil {
		t.Fatalf("seed hide meta: %v", err)
	}

	visPath := mount + "/vis/a.txt"
	hiddenPath := mount + "/vis/hidden.txt" // node anchored on demand; no disk file needed
	if err := op.ApplyLabel(context.Background(), visPath, red.ID); err != nil {
		t.Fatal(err)
	}
	if err := op.ApplyLabel(context.Background(), hiddenPath, red.ID); err != nil {
		t.Fatal(err)
	}

	w := driveParam(u, strconv.Itoa(int(red.ID)), LabelFiles)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("LabelFiles must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, visPath) {
		t.Fatalf("LabelFiles must list the visible file %q: %s", visPath, body)
	}
	if strings.Contains(body, hiddenPath) {
		t.Fatalf("LabelFiles leaked a hide-meta'd path %q (Authorize hide dimension not applied): %s", hiddenPath, body)
	}
}

// TestLabelsOfHonorsHideMeta: reading the labels of a hidden-by-meta file must
// be refused (Authorize ActRead applies the hide rule), even though the file is
// inside the caller's readable scope.
func TestLabelsOfHonorsHideMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, red, mount := setupLabelFixture(t)
	if err := op.CreateMeta(&model.Meta{Path: mount + "/vis", Hide: "hidden.*", HSub: true}); err != nil {
		t.Fatalf("seed hide meta: %v", err)
	}
	hiddenPath := mount + "/vis/hidden.txt"
	if err := op.ApplyLabel(context.Background(), hiddenPath, red.ID); err != nil {
		t.Fatal(err)
	}
	w := driveQuery(u, hiddenPath, LabelsOf)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("LabelsOf on a hide-meta'd path must be 403, got %d: %s", got, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Attack 5 (consistency/race): concurrent apply/remove of the same (label,node)
// and delete-vs-apply must not corrupt state or panic. Run with -race. Idempotent
// binding (OnConflict) + cascade delete are the invariants under test.
// ---------------------------------------------------------------------------

func TestLabelConcurrentApplyRemove(t *testing.T) {
	_, red, mount := setupLabelFixture(t)
	p := mount + "/vis/a.txt"

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = op.ApplyLabel(context.Background(), p, red.ID)
			} else {
				_ = op.RemoveLabel(context.Background(), p, red.ID)
			}
		}(i)
	}
	wg.Wait()

	// Whatever the interleaving, the final state must be internally consistent:
	// either 0 or 1 binding of red on the node (never a duplicate).
	labels, err := op.LabelsOf(p)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, l := range labels {
		if l.ID == red.ID {
			count++
		}
	}
	if count > 1 {
		t.Fatalf("concurrent apply produced %d duplicate bindings (unique index not holding)", count)
	}
}

// TestLabelConcurrentDeleteVsApply races a vocabulary delete against applies of
// the same label. After the dust settles the label is gone and no readable
// binding survives to a since-deleted label (LabelsByNode INNER-joins, so a
// stray binding row would simply not surface as a label — assert that).
func TestLabelConcurrentDeleteVsApply(t *testing.T) {
	_, red, mount := setupLabelFixture(t)
	p := mount + "/vis/a.txt"

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = op.ApplyLabel(context.Background(), p, red.ID)
		}
	}()
	go func() {
		defer wg.Done()
		_ = op.DeleteLabel(red.ID)
	}()
	wg.Wait()

	// The label vocabulary entry must be gone.
	if _, err := op.GetLabel(red.ID); err == nil {
		// A late apply cannot resurrect the vocabulary row; GetLabel must fail.
		t.Fatalf("deleted label still resolvable after concurrent apply")
	}
	// And no label surfaces on the node for the deleted id.
	labels, err := op.LabelsOf(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range labels {
		if l.ID == red.ID {
			t.Fatalf("deleted label still surfaces on node via LabelsOf: %+v", l)
		}
	}
}
