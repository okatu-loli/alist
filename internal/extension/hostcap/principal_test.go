package hostcap

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// MED-1 staleness: a principal issued for an enabled user must STOP authorizing
// once that user is disabled — the handle re-resolves live state, it does not
// carry a frozen snapshot that keeps a disabled user passing.
func TestPrincipalDisabledAfterIssueDenied(t *testing.T) {
	mnt := "/prin-disable"
	mountLocal(t, mnt)
	ctx := context.Background()

	if err := op.CreateUser(&model.User{Username: "prin-disable-user", Disabled: false}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	u, err := op.GetUserByName("prin-disable-user")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	h := New(Grant{
		PluginID: "p.prin.disable",
		HostAPIs: []GrantItem{{Name: CapFS}},
	}, Options{})
	prin := h.auth.issuePrincipal(u)

	// While enabled the handle resolves to the live user.
	if got := h.prin.lookup(string(prin)); got == nil || got.ID != u.ID {
		t.Fatalf("handle for an enabled user must resolve to it, got %+v", got)
	}

	// Disable the user; the principal handle must now resolve to nil (unknown).
	if err := op.UpdateUser(&model.User{ID: u.ID, Username: u.Username, Disabled: true}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if got := h.prin.lookup(string(prin)); got != nil {
		t.Fatalf("principal for a now-disabled user must resolve to nil, got %+v", got)
	}
	// And any FS op through that principal is denied.
	if _, err := h.FS().Resolve(ctx, prin, mnt); err != ext.ErrPermissionDenied {
		t.Fatalf("FS via disabled-user principal: want ErrPermissionDenied, got %v", err)
	}
}

// MED-1 unbounded map: issuing more than the cap must not grow the map past it;
// the oldest handles are evicted.
func TestPrincipalMapCapEnforced(t *testing.T) {
	p := &principals{m: map[string]principalRef{}, order: nil, cap: 8}
	u := &model.User{ID: 1, Username: "cap-user"}

	var first string
	handles := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		h := p.issue(u)
		if i == 0 {
			first = h
		}
		handles = append(handles, h)
	}
	if len(p.m) > p.cap {
		t.Fatalf("map size %d exceeds cap %d", len(p.m), p.cap)
	}
	// The very first (oldest) handle must have been evicted.
	if _, ok := p.m[first]; ok {
		t.Fatal("oldest handle should have been evicted once the cap was hit")
	}
	// The most recent cap handles are retained.
	if _, ok := p.m[handles[len(handles)-1]]; !ok {
		t.Fatal("most recently issued handle must be retained")
	}
}
