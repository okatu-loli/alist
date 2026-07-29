package op

import (
	"context"
	stdpath "path"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/pkg/utils/random"
)

// shareTokenLen is the length of the crypto/rand share token (62^32 entropy),
// unguessable by construction; the DB unique index guarantees no collisions.
const shareTokenLen = 32

// CreateShare mints a public share for the object at vpath. The path is cleaned
// and resolved to its (storage, relpath) anchor, whose node chain is created on
// demand (EnsureNodeChain) — the same anchoring labels/permissions/metas use —
// so the share follows the target when it moves. A crypto/rand token is drawn,
// the optional password is stored only as a salted one-way hash, and an
// optional expiry is computed from expireHours (<=0 means never). The HTTP layer
// gates this on read permission (op.Authorize ActRead) before calling; op itself
// only anchors and persists.
func CreateShare(ctx context.Context, creatorID uint, vpath, password string, expireHours int) (*model.Share, error) {
	ref, err := ResolvePathAnchor(utils.FixAndCleanPath(vpath))
	if err != nil {
		return nil, err
	}
	nodeID, err := db.EnsureNodeChain(ref.StorageID, ref.Anchor)
	if err != nil {
		return nil, err
	}

	passwordHash := ""
	if password != "" {
		passwordHash, err = utils.HashToken(password)
		if err != nil {
			return nil, err
		}
	}

	var expiresAt *time.Time
	if expireHours > 0 {
		t := time.Now().Add(time.Duration(expireHours) * time.Hour)
		expiresAt = &t
	}

	s := &model.Share{
		Token:        random.String(shareTokenLen),
		NodeID:       nodeID,
		CreatorID:    creatorID,
		PasswordHash: passwordHash,
		ExpiresAt:    expiresAt,
	}
	if err := db.CreateShare(s); err != nil {
		return nil, err
	}
	return s, nil
}

// ResolveShare maps a bearer token to its live share and current share-root
// virtual path. Non-live shares — missing, disabled or expired — all collapse to
// errs.ShareNotFound so the public layer answers with a single 404 that leaks no
// distinction between them (enumeration defense). The share root is re-derived
// from the anchored node via NodeVirtualPath, so it tracks moves/renames of the
// shared target for free.
func ResolveShare(token string) (share *model.Share, shareRoot string, err error) {
	s, err := db.GetShareByToken(token)
	if err != nil {
		return nil, "", err // includes errs.ShareNotFound for a missing token
	}
	if s.Disabled || (s.ExpiresAt != nil && time.Now().After(*s.ExpiresAt)) {
		return nil, "", errs.ShareNotFound
	}
	root, err := NodeVirtualPath(s.NodeID)
	if err != nil {
		return nil, "", err
	}
	return s, root, nil
}

// ShareSubPath confines sub within the share subtree rooted at shareRoot and
// returns the resolved absolute virtual path. This is the single confinement
// gate: sub is joined onto the root and cleaned (FixAndCleanPath collapses "..",
// backslashes and duplicate slashes and normalizes the leading "/"), then the
// result must be exactly shareRoot or a descendant (prefix shareRoot+"/") — any
// escape (relative "..", absolute path outside the root, encoded traversal)
// yields errs.ShareOutOfScope. sub == "" resolves to the root itself.
//
// SECURITY: sub is relative to the share root by contract (the public handlers
// pass a client-supplied ?sub= that is a path within the share). An absolute sub
// (leading "/" or "\") is malformed/hostile — reject it up front rather than let
// Join silently reinterpret it as relative. The HasPrefix check below is the
// load-bearing traversal gate: removing it would let a crafted "../secret"
// resolve to a path outside the shared subtree, exposing files the share never
// granted.
func ShareSubPath(shareRoot, sub string) (string, error) {
	if norm := strings.ReplaceAll(sub, "\\", "/"); strings.HasPrefix(norm, "/") {
		return "", errs.ShareOutOfScope
	}
	clean := utils.FixAndCleanPath(stdpath.Join(shareRoot, sub))
	if clean != shareRoot && !strings.HasPrefix(clean, shareRoot+"/") {
		return "", errs.ShareOutOfScope
	}
	return clean, nil
}
