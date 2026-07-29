package _123Open

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// 123_open rapid-upload ("秒传") tests (design SP-H / H1): TryRapidUpload uses
// the whole-file MD5 the SOURCE already exposes to call the create/reuse API —
// it must NEVER read or spool bytes to compute a hash itself. A create response
// with reuse=true is a hit; reuse=false is a miss (stream instead); no usable
// hash is errs.NotSupport (the API's reuse check is keyed on the MD5 etag).

// TestTryRapidUploadMD5ReuseHit: given an MD5, the driver calls create and a
// reuse=true response is a hit returning the new object.
func TestTryRapidUploadMD5ReuseHit(t *testing.T) {
	fs := newFakeServer(t)
	fs.createHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data":    map[string]any{"fileId": 4242, "reuse": true},
		})
	}
	d := fs.newDriver()

	hashes := utils.NewHashInfo(utils.MD5, "0123456789abcdef0123456789abcdef")
	obj, hit, err := d.TryRapidUpload(context.Background(), &model.Object{ID: "0"}, "reuse.bin", 100, hashes)
	if err != nil {
		t.Fatalf("TryRapidUpload: %v", err)
	}
	if !hit {
		t.Fatal("reuse=true must be reported as a rapid hit")
	}
	if obj == nil || obj.GetName() != "reuse.bin" || obj.GetSize() != 100 {
		t.Fatalf("hit obj = %+v, want name=reuse.bin size=100", obj)
	}
	if obj.GetID() != "4242" {
		t.Fatalf("hit obj id = %q, want 4242 (from the reuse response)", obj.GetID())
	}
}

// TestTryRapidUploadReuseFalseMiss: a create response with reuse=false is a miss
// — (nil,false,nil) — so the caller streams the bytes instead.
func TestTryRapidUploadReuseFalseMiss(t *testing.T) {
	fs := newFakeServer(t)
	fs.createHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data":    map[string]any{"fileId": 0, "reuse": false, "preuploadId": "p1", "sliceSize": 8, "servers": []string{fs.server.URL}},
		})
	}
	d := fs.newDriver()

	hashes := utils.NewHashInfo(utils.MD5, "0123456789abcdef0123456789abcdef")
	obj, hit, err := d.TryRapidUpload(context.Background(), &model.Object{ID: "0"}, "miss.bin", 100, hashes)
	if err != nil {
		t.Fatalf("a miss must not be an error, got %v", err)
	}
	if hit || obj != nil {
		t.Fatalf("reuse=false must be a miss (nil,false,nil), got obj=%+v hit=%v", obj, hit)
	}
}

// TestTryRapidUploadNoMD5NotSupport: with no MD5 in the provided hashes the
// driver returns errs.NotSupport and NEVER calls the create API (it must not
// spool/compute a hash to force a rapid attempt).
func TestTryRapidUploadNoMD5NotSupport(t *testing.T) {
	fs := newFakeServer(t)
	d := fs.newDriver()

	// A sha1-only HashInfo — 123's reuse check is MD5(etag)-keyed, so this is
	// unusable for rapid upload here.
	hashes := utils.NewHashInfo(utils.SHA1, "da39a3ee5e6b4b0d3255bfef95601890afd80709")
	obj, hit, err := d.TryRapidUpload(context.Background(), &model.Object{ID: "0"}, "nohash.bin", 100, hashes)
	if !errors.Is(err, errs.NotSupport) {
		t.Fatalf("no usable hash must return errs.NotSupport, got %v", err)
	}
	if hit || obj != nil {
		t.Fatalf("NotSupport must not report a hit, got obj=%+v hit=%v", obj, hit)
	}
	if got := atomic.LoadInt32(&fs.createCalls); got != 0 {
		t.Fatalf("create must NOT be called when there is no usable hash (never spool to compute one), called %d times", got)
	}
}
