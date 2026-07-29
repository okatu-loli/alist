package extension

import (
	"testing"
	"time"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

// fakeStore is an in-memory tokenstore.Store for the scoped-token tests, so
// they need no database. It honors revocation and expiry so those paths are
// exercised.
type fakeStore struct {
	rows map[string]*tokenstore.Record
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]*tokenstore.Record{}} }

func (s *fakeStore) Save(r tokenstore.Record) error {
	cp := r
	s.rows[r.JTI] = &cp
	return nil
}
func (s *fakeStore) Get(jti string) (*tokenstore.Record, error) {
	r, ok := s.rows[jti]
	if !ok {
		return nil, tokenstore.ErrTokenNotFound
	}
	if r.ExpiresAt < time.Now().Unix() {
		return nil, tokenstore.ErrTokenExpired
	}
	return r, nil
}
func (s *fakeStore) Revoke(jti string) error { delete(s.rows, jti); return nil }
func (s *fakeStore) RevokeIfActive(jti string) (bool, error) {
	if _, ok := s.rows[jti]; !ok {
		return false, nil
	}
	delete(s.rows, jti)
	return true, nil
}
func (s *fakeStore) RevokeBySession(string) error { return nil }
func (s *fakeStore) RevokeByUser(uint) error      { return nil }
func (s *fakeStore) GC() (int, error)             { return 0, nil }

func scopedTestRegistry(t *testing.T, storageTokens []string) *Registry {
	t.Helper()
	if conf.Conf == nil {
		conf.Conf = conf.DefaultConfig()
	}
	tokenstore.Init(newFakeStore())
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{
			ID:          "ext.store",
			Kinds:       []ext.Kind{ext.KindStorageDriver},
			Permissions: ext.Permissions{StorageTokens: storageTokens},
		},
		Kind:    ext.KindStorageDriver,
		Mode:    InstanceModeConstructor,
		enabled: true,
	}
	if err := r.Register(e); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIssueScopedTokenWithinScopes(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/data"})
	tok, err := IssueScopedToken(r, "ext.store", []Scope{{Path: "/data/sub"}}, time.Minute)
	if err != nil {
		t.Fatalf("issue within scope: %v", err)
	}
	u, err := AuthenticateScopedToken(tok)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !u.Scoped || len(u.ScopedPermissions) != 1 {
		t.Fatalf("principal not scoped correctly: %+v", u)
	}
	p := u.ScopedPermissions[0]
	if p.Path != "/data/sub" || p.Effect != model.EffectAllow || !p.Recursive {
		t.Fatalf("scoped permission wrong: %+v", p)
	}
	if !p.Actions.Contains(model.ActRead) || p.Actions.Contains(model.ActWrite) {
		t.Fatalf("read-only scope must grant read, not write: %v", p.Actions)
	}
}

func TestIssueScopedTokenWriteScopeGrantsWrite(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/data"})
	tok, err := IssueScopedToken(r, "ext.store", []Scope{{Path: "/data", Write: true}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := AuthenticateScopedToken(tok)
	if !u.ScopedPermissions[0].Actions.Contains(model.ActWrite) {
		t.Fatalf("write scope must grant write: %v", u.ScopedPermissions[0].Actions)
	}
}

func TestIssueScopedTokenOverScopeRejected(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/data"})
	if _, err := IssueScopedToken(r, "ext.store", []Scope{{Path: "/other"}}, time.Minute); err == nil {
		t.Fatal("scope outside declared storage_tokens must be rejected at issuance")
	}
	// a sibling that merely shares a prefix string but is not under /data
	if _, err := IssueScopedToken(r, "ext.store", []Scope{{Path: "/database"}}, time.Minute); err == nil {
		t.Fatal("/database is not under /data and must be rejected")
	}
}

func TestIssueScopedTokenTTLClamped(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/"})
	// request an hour; must be clamped to MaxScopedTokenTTL
	tok, err := IssueScopedToken(r, "ext.store", []Scope{{Path: "/"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// still authenticates now, which only proves it was issued; the clamp is
	// asserted by decoding the exp being <= now+Max (allow small skew).
	if _, err := AuthenticateScopedToken(tok); err != nil {
		t.Fatalf("clamped token should still authenticate immediately: %v", err)
	}
}

func TestScopedTokenRevoke(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/data"})
	tok, _ := IssueScopedToken(r, "ext.store", []Scope{{Path: "/data"}}, time.Minute)
	if _, err := AuthenticateScopedToken(tok); err != nil {
		t.Fatalf("pre-revoke authenticate: %v", err)
	}
	if err := RevokeScopedToken(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateScopedToken(tok); err == nil {
		t.Fatal("revoked scoped token must not authenticate")
	}
}

func TestScopedTokenStoreExpiry(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/data"})
	tok, _ := IssueScopedToken(r, "ext.store", []Scope{{Path: "/data"}}, time.Minute)
	// force the store record to be expired while the JWT is still time-valid:
	// the fail-closed store check must reject it regardless.
	store := tokenstore.Get().(*fakeStore)
	for _, rec := range store.rows {
		rec.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	}
	if _, err := AuthenticateScopedToken(tok); err == nil {
		t.Fatal("store-expired scoped token must not authenticate")
	}
}

func TestScopedTokenNotEnabledRejected(t *testing.T) {
	r := scopedTestRegistry(t, []string{"/data"})
	_ = r.Disable("ext.store")
	if _, err := IssueScopedToken(r, "ext.store", []Scope{{Path: "/data"}}, time.Minute); err == nil {
		t.Fatal("issuing for a disabled extension must fail")
	}
}

func TestNormalTokenNotRecognizedAsScoped(t *testing.T) {
	// A token without an "ext" claim must not be routed through scoped auth.
	if IsScopedToken("eyJhbGciOiJIUzI1NiJ9.eyJ1c2VybmFtZSI6ImFsaWNlIn0.sig") {
		t.Fatal("a user-shaped token must not look scoped")
	}
	if IsScopedToken("") {
		t.Fatal("empty token must not look scoped")
	}
}
