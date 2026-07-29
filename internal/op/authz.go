package op

import (
	"context"
	stdpath "path"
	"strings"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/dlclark/regexp2"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// AuthzRequest carries the request-scoped inputs of one authorization
// decision. ResourceType selects the matching domain: "" or "fs" targets the
// filesystem (Path is the full virtual path, already base-path joined);
// "system" targets an admin control-plane domain (Path is the domain
// selector). Password only applies to fs reads.
type AuthzRequest struct {
	ResourceType string
	Path         string
	Password     string
}

// systemDomains is the whitelist of admin control-plane domains a system
// permission may target. Requests outside it fast-fail before any policy
// lookup (spec §2.4).
var systemDomains = map[string]struct{}{
	"storage": {}, "user": {}, "role": {}, "permission": {}, "driver": {},
	"setting": {}, "signing_key": {}, "session": {}, "meta": {},
	"offline_download": {}, "task": {},
}

// CanonicalDomain normalizes a system domain selector (lowercase, trimmed) and
// reports whether it is a recognized domain.
func CanonicalDomain(s string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(s))
	_, ok := systemDomains[d]
	return d, ok
}

// isFS reports whether a resource type is the filesystem domain. "" defaults
// to fs so existing fs callers/rows need no ResourceType set.
func isFS(rt string) bool { return rt == "" || rt == model.ResourceFS }

// Authorize is the single decision entrypoint of Step 4: policy (deny-first
// RBAC5) + chain-merged meta (password/hide/write) in one place. Every fs
// entry must call this instead of composing role_perm.go primitives.
//
// Being the choke point also makes it the audit source (Step 12): every
// decision — allow and deny alike — is recorded asynchronously. Engine
// failures audit as denials; they deny the request either way.
func Authorize(ctx context.Context, u *model.User, act model.Action, req AuthzRequest) error {
	err := authorize(ctx, u, act, req)
	audit.Decision(ctx, u, auditAction(act, req), auditTarget(req), err == nil)
	return err
}

// auditAction renders the dotted audit verb: system acts already carry the
// "system.<domain>.<verb>" form; fs acts gain an "fs." family prefix.
func auditAction(act model.Action, req AuthzRequest) string {
	if req.ResourceType == model.ResourceSystem {
		return string(act)
	}
	return "fs." + string(act)
}

func auditTarget(req AuthzRequest) string {
	if req.ResourceType == model.ResourceSystem {
		return "system:" + req.Path
	}
	return req.Path
}

func authorize(ctx context.Context, u *model.User, act model.Action, req AuthzRequest) error {
	if u == nil || u.Disabled {
		return errors.WithStack(errs.PermissionDenied)
	}
	if req.ResourceType == model.ResourceSystem {
		// system domain: exact-match decision only — no meta/hide/password or
		// Traverse escapes apply to the admin control plane.
		dom, ok := CanonicalDomain(req.Path)
		if !ok {
			return errors.WithStack(errs.PermissionDenied)
		}
		d, err := resolvePolicy(u, AuthzRequest{ResourceType: model.ResourceSystem, Path: dom})
		if err != nil {
			return err
		}
		if d.Can(act) {
			return nil
		}
		return errors.WithStack(errs.PermissionDenied)
	}
	reqPath := utils.FixAndCleanPath(req.Path)
	d, err := ResolveEffectivePolicy(u, reqPath)
	if err != nil {
		return err
	}
	// path-limit is an opt-in restriction bit: when present, the path must be
	// inside some granted scope (replicates the retired v3 path-limit check).
	if d.Can(model.ActPathLimit) && !d.Can(model.ActRead) && !d.Traverse {
		return errors.WithStack(errs.PermissionDenied)
	}
	switch act {
	case model.ActRead:
		// An explicit deny-read must beat the Traverse escape: an exact-match
		// allow on a deeper path must not resurrect access to a path already
		// covered by a recursive deny above it.
		if d.Denied(model.ActRead) {
			return errors.WithStack(errs.PermissionDenied)
		}
		if !d.Can(model.ActRead) && !d.Traverse {
			return errors.WithStack(errs.PermissionDenied)
		}
		em, err := ResolveEffectiveMeta(reqPath)
		if err != nil {
			return err
		}
		if !d.Can(model.ActSeeHides) && matchAnyHide(em.HidePatterns, stdpath.Base(reqPath)) {
			return errors.WithStack(errs.PermissionDenied)
		}
		if em.PasswordApplies && !d.Can(model.ActAccessWithoutPwd) && req.Password != em.Password {
			return errors.WithStack(errs.WrongPassword)
		}
		return nil
	case model.ActWrite:
		if d.Can(model.ActWrite) {
			return nil
		}
		// An explicit deny-write must not be overridden by the directory
		// meta.Write fallback below.
		if d.Denied(model.ActWrite) {
			return errors.WithStack(errs.PermissionDenied)
		}
		em, err := ResolveEffectiveMeta(reqPath)
		if err != nil {
			return err
		}
		if em.Write {
			return nil
		}
		return errors.WithStack(errs.PermissionDenied)
	default:
		if d.Can(act) {
			return nil
		}
		return errors.WithStack(errs.PermissionDenied)
	}
}

// FilterListing applies per-child read policy and hide rules in ONE place, so
// no entry point can forget per-child filtering (defect #10). With the
// fs.list hide seam removed (Step 5), this is the sole hide enforcement point
// for directory listings.
func FilterListing(ctx context.Context, u *model.User, dirPath string, objs []model.Obj) []model.Obj {
	dirPath = utils.FixAndCleanPath(dirPath)
	patterns, err := EffectiveHidePatterns(dirPath)
	return filterWithPatterns(u, dirPath, objs, patterns, err)
}

// filterWithPatterns applies per-child read policy and hide rules; a hide-rule
// resolution failure fails CLOSED (empty listing) — with the fs.list hide seam
// removed (Step 5) this is the only hide enforcement point.
func filterWithPatterns(u *model.User, dirPath string, objs []model.Obj, patterns []string, patternsErr error) []model.Obj {
	if patternsErr != nil {
		log.Errorf("FilterListing: hide patterns for %s failed, failing closed: %+v", dirPath, patternsErr)
		return []model.Obj{}
	}
	out := make([]model.Obj, 0, len(objs))
	for _, obj := range objs {
		childPath := stdpath.Join(dirPath, obj.GetName())
		d, err := ResolveEffectivePolicy(u, childPath)
		if err != nil {
			log.Errorf("FilterListing: policy for %s: %+v", childPath, err)
			continue // fail closed
		}
		// Same deny-first-beats-Traverse reasoning as Authorize's ActRead branch.
		if d.Denied(model.ActRead) {
			continue
		}
		if !d.Can(model.ActRead) && !d.Traverse {
			continue
		}
		if !d.Can(model.ActSeeHides) && matchAnyHide(patterns, obj.GetName()) {
			continue
		}
		out = append(out, obj)
	}
	return out
}

func matchAnyHide(patterns []string, name string) bool {
	for _, pat := range patterns {
		re, err := regexp2.Compile(pat, regexp2.None)
		if err != nil {
			continue // malformed pattern: same fail-open as legacy MustCompile-adjacent behavior, but without panicking
		}
		if ok, _ := re.MatchString(name); ok {
			return true
		}
	}
	return false
}
