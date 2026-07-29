package op

import (
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/pkg/errors"
)

// PolicyDiff is one behavior difference between the legacy (v3) semantics and
// the Step 4 engine at a concrete path. Consumed by `alist migrate v4
// --dry-run` (WS-7) to warn admins before upgrading.
type PolicyDiff struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// PolicyDiffReport compares frozen legacy semantics against the live engine
// for the given user over the given paths. The legacy reference below is a
// re-implementation (scopes + nearest meta), NOT the deprecated wrappers —
// those already delegate to the engine.
func PolicyDiffReport(u *model.User, paths []string) ([]PolicyDiff, error) {
	var out []PolicyDiff
	for _, p := range paths {
		p = utils.FixAndCleanPath(p)

		// 1. ancestor password: legacy asks the NEAREST meta only.
		legacyPw, err := legacyPasswordApplies(p)
		if err != nil {
			return nil, err
		}
		em, err := ResolveEffectiveMeta(p)
		if err != nil {
			return nil, err
		}
		if em.PasswordApplies && !legacyPw {
			out = append(out, PolicyDiff{Path: p, Kind: "ancestor-password-now-applies",
				Detail: "an ancestor meta password now protects this path; v3's nearest-meta lookup skipped it"})
		}

		// 2. root union quirk: at "/" (or base path) legacy unioned all scope
		// bits; the engine only grants what matches.
		if p == "/" {
			legacyBits := legacyMergeBits(u, p)
			d, err := ResolveEffectivePolicy(u, p)
			if err != nil {
				return nil, err
			}
			if legacyBits != d.LegacyBits() {
				out = append(out, PolicyDiff{Path: p, Kind: "root-union-quirk",
					Detail: "v3 unioned every scope's bits at the root/base path; the engine matches by path"})
			}
		}
	}
	return out, nil
}

// legacyPasswordApplies reproduces v3: nearest meta only, IsApply semantics.
func legacyPasswordApplies(reqPath string) (bool, error) {
	meta, err := GetNearestMeta(reqPath)
	if err != nil {
		if errors.Is(err, errs.MetaNotFound) || errors.Cause(err) == errs.MetaNotFound {
			return false, nil
		}
		return false, err
	}
	if meta.Password == "" {
		return false, nil
	}
	return utils.PathEqual(meta.Path, reqPath) || meta.PSub, nil
}

// legacyMergeBits reproduces the pre-delegation v3 root-union behavior
// (union of every role scope's raw legacy bits) directly from role scopes.
func legacyMergeBits(u *model.User, reqPath string) int32 {
	if u == nil {
		return 0
	}
	var bits int32
	for _, rid := range u.Role {
		role, err := GetRole(uint(rid))
		if err != nil {
			continue
		}
		for _, entry := range role.PermissionScopes {
			if reqPath == "/" ||
				utils.IsSubPath(entry.Path, reqPath) {
				bits |= entry.Permission
			}
		}
	}
	return bits
}
