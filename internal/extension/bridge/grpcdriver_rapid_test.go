package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urldriver"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// TestPluginDriverRapidRoundTrip exercises pluginDriver.TryRapidUpload (SP-H)
// against the real urlplugin gRPC subprocess, which declares CapPutRapid and
// implements the synthetic magic-md5 rapid semantics (urldriver.go). It
// covers both outcomes over the real wire: the magic md5 hits and returns an
// Obj with the "rapid-" ID prefix, any other md5 misses cleanly (nil, false,
// nil) so the caller falls back to streaming.
func TestPluginDriverRapidRoundTrip(t *testing.T) {
	bin := buildURLPlugin(t)
	pm := pluginproc.New()
	caps := []ext.Capability{ext.CapGetter, ext.CapPutRapid}

	d := NewPluginDriver("com.example.urldriver.rapid", bin, caps, false, pm)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.urldriver.rapid"})

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Drop(ctx) })

	rapid, ok := d.(driver.PutRapid)
	if !ok {
		t.Fatal("pluginDriver must implement driver.PutRapid")
	}

	dstDir := &model.Object{Path: "/", IsFolder: true}

	// Hit: the magic md5.
	hitHashes := utils.NewHashInfo(utils.MD5, urldriver.RapidMagicMD5)
	obj, hit, err := rapid.TryRapidUpload(ctx, dstDir, "f.bin", 42, hitHashes)
	if err != nil {
		t.Fatalf("TryRapidUpload (hit): %v", err)
	}
	if !hit {
		t.Fatal("TryRapidUpload (hit) reported miss")
	}
	if obj == nil || obj.GetID() != "rapid-f.bin" {
		t.Fatalf("TryRapidUpload (hit) obj = %+v, want ID rapid-f.bin", obj)
	}

	// Miss: any other md5.
	missHashes := utils.NewHashInfo(utils.MD5, "deadbeefdeadbeefdeadbeefdeadbeef")
	obj, hit, err = rapid.TryRapidUpload(ctx, dstDir, "f.bin", 42, missHashes)
	if err != nil {
		t.Fatalf("TryRapidUpload (miss): %v", err)
	}
	if hit {
		t.Fatalf("TryRapidUpload (miss) reported hit: %+v", obj)
	}

	// Hit with an empty Obj (M1): the plugin reports a hit but hands back no
	// identifying object at all. pluginDriver must still report hit=true, but
	// must return a nil model.Obj rather than extObjToModel's all-zero-value
	// *model.Object — see TryRapidUpload's doc for why a non-nil ghost object
	// here would get wrongly cached by op.PutRapid as a real entry instead of
	// correctly invalidating the destination dir's cache.
	emptyObjHashes := utils.NewHashInfo(utils.MD5, urldriver.RapidMagicMD5EmptyObj)
	obj, hit, err = rapid.TryRapidUpload(ctx, dstDir, "f.bin", 42, emptyObjHashes)
	if err != nil {
		t.Fatalf("TryRapidUpload (hit, empty obj): %v", err)
	}
	if !hit {
		t.Fatal("TryRapidUpload (hit, empty obj) reported miss")
	}
	if obj != nil {
		t.Fatalf("TryRapidUpload (hit, empty obj) obj = %+v, want nil", obj)
	}
}

// TestPluginDriverRapidNotImplementedWithoutCapability confirms the
// capability gate: localplugin (the local driver bridged over gRPC) never
// declares CapPutRapid, so TryRapidUpload must fail with errs.NotImplement
// rather than silently no-op'ing, mirroring
// TestPluginDriverPutNotImplementedWithoutCapability one op over.
func TestPluginDriverRapidNotImplementedWithoutCapability(t *testing.T) {
	withTempDir(t)
	bin := buildLocalPlugin(t)
	pm := pluginproc.New()
	root := t.TempDir()

	d := NewPluginDriver("com.example.localdriver.rapid", bin, nil, true, pm)
	d.SetStorage(model.Storage{ID: 1, Driver: "com.example.localdriver.rapid"})

	cfg, _ := json.Marshal(map[string]any{
		"root_folder_path": root,
		"mkdir_perm":       "777",
		"recycle_bin_path": "delete permanently",
	})
	if err := json.Unmarshal(cfg, d.GetAddition()); err != nil {
		t.Fatalf("unmarshal addition: %v", err)
	}

	ctx := context.Background()
	if err := d.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Drop(ctx) })

	rapid, ok := d.(driver.PutRapid)
	if !ok {
		t.Fatal("pluginDriver must implement driver.PutRapid")
	}

	hashes := utils.NewHashInfo(utils.MD5, urldriver.RapidMagicMD5)
	obj, hit, err := rapid.TryRapidUpload(ctx, &model.Object{Path: root, IsFolder: true}, "f.bin", 42, hashes)
	if !errors.Is(err, errs.NotImplement) {
		t.Fatalf("TryRapidUpload on non-declaring plugin err = %v, want errs.NotImplement", err)
	}
	if hit || obj != nil {
		t.Fatalf("TryRapidUpload on non-declaring plugin = (%+v, %v), want (nil, false)", obj, hit)
	}
}
