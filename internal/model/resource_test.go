package model

import "testing"

func TestResourceRefCanonicalStable(t *testing.T) {
	a := ResourceRef{StorageID: 3, Anchor: "/x/y", AnchorKind: AnchorRelPath}
	b := ResourceRef{StorageID: 3, Anchor: "/x/y", AnchorKind: AnchorRelPath}
	if a.Canonical() != b.Canonical() {
		t.Fatal("same ref must canonicalize identically")
	}
	if a.Canonical() == (ResourceRef{StorageID: 4, Anchor: "/x/y", AnchorKind: AnchorRelPath}).Canonical() {
		t.Fatal("different storage must differ")
	}
	if a.Canonical() == (ResourceRef{StorageID: 3, Anchor: "/x/z", AnchorKind: AnchorRelPath}).Canonical() {
		t.Fatal("different anchor must differ")
	}
}
