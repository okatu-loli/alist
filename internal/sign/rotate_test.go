package sign

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
)

func TestRotateThenGrace(t *testing.T) {
	initSignTestDB(t) // reuse keyset_test helper (same package)
	tok := Sign("/1/a")
	kid, err := Rotate(model.SignPurposeDownload)
	if err != nil || kid == "" {
		t.Fatalf("rotate: %v", err)
	}
	if err := Verify("/1/a", tok); err != nil {
		t.Fatal("old token must survive rotation (grace)")
	}
	removed, err := GC(model.SignPurposeDownload)
	if err != nil || removed < 1 {
		t.Fatalf("gc: %d %v", removed, err)
	}
	if err := Verify("/1/a", tok); err == nil {
		t.Fatal("after gc old token must fail")
	}
}

func TestRotateRejectsUnwiredPurpose(t *testing.T) {
	initSignTestDB(t)
	if _, err := Rotate(model.SignPurposeArchive); err == nil {
		t.Fatal("archive purpose is not wired in Step 6, rotate must reject")
	}
}
