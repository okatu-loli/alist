package op

import (
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// resolvePathAnchor resolves a virtual path exactly once and returns both the
// live storage handle and the relpath-anchored ref, so callers never need to
// re-resolve: balance groups round-robin across their member storages
// (GetBalancedStorage), so a second resolution of the same path can return a
// DIFFERENT storage than the first, producing a ref whose StorageID and
// Anchor belong to two different drivers. The live handle is returned as-is
// (whichever member GetBalancedStorage picked, for driver calls like Get);
// ref.StorageID is normalized to the group's primary member
// (balanceGroupPrimary) so a balance-anchored ref is stable across calls
// regardless of which member actually served this resolution.
func resolvePathAnchor(virtualPath string) (driver.Driver, string, model.ResourceRef, error) {
	storage, actualPath, err := GetStorageAndActualPath(virtualPath)
	if err != nil {
		return nil, "", model.ResourceRef{}, err // wraps errs.StorageNotFound for virtual paths
	}
	ref := model.ResourceRef{
		StorageID:  balanceGroupPrimary(storage).GetStorage().ID,
		Anchor:     utils.FixAndCleanPath(actualPath),
		AnchorKind: model.AnchorRelPath,
	}
	return storage, actualPath, ref, nil
}

// ResolvePathAnchor maps a virtual path to a relpath-anchored ResourceRef
// using mount points only — no driver call, safe before storages are loaded.
// Virtual paths above every mount point return errs.StorageNotFound: such
// metas legitimately stay path-anchored (they cannot be moved by fs ops).
func ResolvePathAnchor(virtualPath string) (model.ResourceRef, error) {
	_, _, ref, err := resolvePathAnchor(virtualPath)
	return ref, err
}
