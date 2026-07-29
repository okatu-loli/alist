package op

import (
	"context"
	"strings"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
)

// subtreeNodeIDs returns rootID plus every descendant node ID, walking
// GetChildNodes breadth-first. A file (or precise anchor) has no children, so
// the result is just rootID — the node-based equivalent of the old "isDir
// decides exact-vs-subtree" branch, now structural: only rows anchored on nodes
// actually beneath rootID are collected, never ones that merely share a string
// prefix.
func subtreeNodeIDs(rootID uint) ([]uint, error) {
	ids := []uint{rootID}
	for i := 0; i < len(ids); i++ {
		children, err := db.GetChildNodes(ids[i])
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// CaptureManualPermissionIDs returns the IDs of the manual permission rows
// anchored on the subtree rooted at srcRel (a storage relpath) on storage —
// exactly the rows a follow relocation carries, and the rows perm_policy=detach
// must delete after a move/rename succeeds.
//
// It is deliberately read-only and called BEFORE the move: a failed move must
// delete nothing, so capture (safe) precedes the move and DeleteDetachedPermissions
// (destructive) follows only on success. Capturing IDs up front also makes the
// deletion robust to a driver that renames the object mid-move — the post-move
// node identity would otherwise have to be re-derived here. Nothing anchored in
// the subtree (no node exists) captures nothing.
func CaptureManualPermissionIDs(ctx context.Context, storage driver.Driver, srcRel string) ([]uint, error) {
	if _, err := GetUnwrap(ctx, storage, srcRel); err != nil {
		return nil, errors.WithMessage(err, "detach: failed to stat source object")
	}
	// balance groups normalize their anchor storage_id to the primary member,
	// so match on that, not the (possibly non-primary) live handle.
	sid := balanceGroupPrimary(storage).GetStorage().ID
	rootID, found, err := db.GetNodeByChain(sid, utils.FixAndCleanPath(srcRel))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	nodeIDs, err := subtreeNodeIDs(rootID)
	if err != nil {
		return nil, err
	}
	ps, err := db.GetManualPermissionsByNodeIDs(nodeIDs)
	if err != nil {
		return nil, err
	}
	ids := make([]uint, len(ps))
	for i := range ps {
		ids[i] = ps[i].ID
	}
	return ids, nil
}

// DeleteDetachedPermissions removes the captured manual permission rows (and
// their role bindings) after a move/rename succeeds, then invalidates the
// policy cache. Empty input is a no-op. Pairs with CaptureManualPermissionIDs.
func DeleteDetachedPermissions(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	if err := db.DeletePermissionsByIDs(ids); err != nil {
		return err
	}
	invalidateAllPolicy()
	return nil
}

// ClonePermissionsForCopy clones every manual permission row anchored in a
// copied source subtree to the copy destination (the copy_perms option, spec
// 0-C). Each clone is re-anchored to a freshly-built destination node chain
// (EnsureNodeChain), keeps Origin=manual and the source Name/DisplayName/
// Actions/Effect/Recursive, gets a fresh ID, and reproduces EVERY role binding
// of its source row (an unbound permission is inert). Scope rows are excluded by
// the manual-only node query.
//
// Call after the copy succeeds. It reads the source (still present after a copy)
// and re-anchors by rebasing each row's node relpath from the source root onto
// the destination root, so the destination objects need not exist yet — the
// copy target subtree, cross-storage or not, is addressed purely by dstStorage
// + dstRel. Invalidates the policy cache when it creates any clone.
func ClonePermissionsForCopy(ctx context.Context, srcStorage driver.Driver, srcRel string, dstStorage driver.Driver, dstRel string) error {
	if _, err := GetUnwrap(ctx, srcStorage, srcRel); err != nil {
		return errors.WithMessage(err, "copy_perms: failed to stat source object")
	}
	srcSID := balanceGroupPrimary(srcStorage).GetStorage().ID
	srcRoot := utils.FixAndCleanPath(srcRel)
	rootID, found, err := db.GetNodeByChain(srcSID, srcRoot)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	nodeIDs, err := subtreeNodeIDs(rootID)
	if err != nil {
		return err
	}
	ps, err := db.GetManualPermissionsByNodeIDs(nodeIDs)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		return nil
	}

	dstSID := balanceGroupPrimary(dstStorage).GetStorage().ID
	dstRoot := utils.FixAndCleanPath(dstRel)

	clones := make([]db.CloneManualPermission, 0, len(ps))
	for i := range ps {
		src := &ps[i]
		// Rebase this row's node relpath from the source subtree root onto the
		// destination root: the top row (relpath == srcRoot) maps to dstRoot, a
		// nested row keeps the relative tail below it.
		_, srcNodeRel, err := NodeRelPath(src.NodeID)
		if err != nil {
			return err
		}
		newRel := utils.FixAndCleanPath(dstRoot + strings.TrimPrefix(srcNodeRel, srcRoot))
		nid, err := db.EnsureNodeChain(dstSID, newRel)
		if err != nil {
			return err
		}
		roleIDs, err := db.GetRoleIDsByPermission(src.ID)
		if err != nil {
			return err
		}
		clones = append(clones, db.CloneManualPermission{
			Permission: model.Permission{
				ResourceType: src.ResourceType,
				Name:         src.Name,
				DisplayName:  src.DisplayName,
				// Node-anchored fs row: the live path derives from NodeID, so no
				// stored Path (matches normalizePermissionResource on create).
				NodeID:    nid,
				Actions:   src.Actions,
				Effect:    src.Effect,
				Recursive: src.Recursive,
				Origin:    model.OriginManual,
			},
			RoleIDs: roleIDs,
		})
	}
	if err := db.ClonePermissionsWithBindings(clones); err != nil {
		return err
	}
	invalidateAllPolicy()
	return nil
}
