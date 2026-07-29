package hostcap

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// maxPrincipals bounds a single plugin Host's live principal handles. Without a
// cap the map grew without limit (MED-1) — every Login/VerifyToken/
// VerifyAppPassword added a row that was never removed, so a chatty (or
// hostile, amplified by the HIGH-1 brute-force) plugin could grow host memory
// unboundedly. When full, issue evicts the oldest handle (FIFO): an old handle
// going dead is harmless because lookup re-resolves live user state anyway.
const maxPrincipals = 4096

// principalRef is the STABLE identity a handle points at — the user id plus the
// username used to re-resolve. It deliberately does NOT freeze the *model.User:
// a snapshot would keep a user who is disabled AFTER issuance passing forever
// (op.Authorize reads Disabled off whatever user it is handed). lookup re-reads
// live state instead.
type principalRef struct {
	uid      uint
	username string
}

// principals maps opaque host-issued handles (ext.Principal strings) to the
// STABLE identity they authenticate as. It is the reason a plugin can hold a
// Principal without being able to read or forge the identity behind it: the
// handle is a random string, and only the host holds the mapping. A protocol
// plugin's Auth().Login/VerifyToken issues a handle here; FS dereferences it,
// re-resolving the user's CURRENT state so a disable/rename/removal after
// issuance takes effect immediately.
type principals struct {
	mu    sync.Mutex
	m     map[string]principalRef
	order []string // FIFO of live handles, for bounded eviction
	cap   int
}

func newPrincipals() *principals {
	return &principals{m: map[string]principalRef{}, cap: maxPrincipals}
}

// issue registers user under a fresh random handle and returns it, evicting the
// oldest handle first if the map is at capacity.
func (p *principals) issue(user *model.User) string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	h := hex.EncodeToString(buf)
	p.mu.Lock()
	if len(p.m) >= p.cap {
		// Pop the front of the FIFO until a still-live handle is removed (skip
		// any already-evicted stragglers), keeping both map and queue bounded.
		for len(p.order) > 0 {
			oldest := p.order[0]
			p.order = p.order[1:]
			if _, ok := p.m[oldest]; ok {
				delete(p.m, oldest)
				break
			}
		}
	}
	p.m[h] = principalRef{uid: user.ID, username: user.Username}
	p.order = append(p.order, h)
	p.mu.Unlock()
	return h
}

// lookup resolves a handle to its CURRENT user, or nil if the handle is unknown
// OR the user it named is now disabled, renamed away, or removed. Re-resolution
// goes through the cached op layer (op.GetUserByName), so it is cheap; the id
// check rejects a username that was freed and re-registered to a different
// account after issuance.
func (p *principals) lookup(handle string) *model.User {
	p.mu.Lock()
	ref, ok := p.m[handle]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	user, err := op.GetUserByName(ref.username)
	if err != nil || user == nil {
		return nil
	}
	if user.ID != ref.uid || user.Disabled {
		return nil
	}
	return user
}

// boundPrincipal builds the restricted *model.User a plugin acts as by default
// (the empty ext.Principal). It mirrors internal/extension.scopedPrincipal: one
// recursive fs allow-permission per granted FSScope (read, plus the write action
// family when the scope grants write), and Scoped=true so op.UserPermissions
// returns exactly these rows — op.Authorize then confines the plugin to exactly
// its granted subtrees, nothing wider. A grant with no FS scopes yields a
// principal that is authorized for nothing (fail-closed): every FS call is
// denied by Authorize.
func boundPrincipal(pluginID string, scopes []FSScope) *model.User {
	perms := make([]model.Permission, 0, len(scopes))
	for _, s := range scopes {
		perms = append(perms, model.Permission{
			ResourceType: model.ResourceFS,
			Name:         "plugin:" + pluginID,
			Path:         utils.FixAndCleanPath(s.Path),
			Actions:      scopeActions(s.Write),
			Effect:       model.EffectAllow,
			Recursive:    true,
			Origin:       model.OriginManual,
		})
	}
	return &model.User{
		Username:          "ext-plugin:" + pluginID,
		Scoped:            true,
		ScopedPermissions: perms,
	}
}

func scopeActions(write bool) model.Actions {
	if !write {
		return model.Actions{model.ActRead}
	}
	return model.Actions{
		model.ActRead, model.ActWrite, model.ActRename,
		model.ActMove, model.ActCopy, model.ActRemove,
	}
}
