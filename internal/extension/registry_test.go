package extension

import (
	"context"
	"errors"
	"testing"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
)

// fakeDriver is a minimal legacy driver.Driver implementing a chosen subset of
// the optional interfaces so capability derivation can be exercised.
type fakeDriver struct {
	model.Storage
	name   string
	mkdir  bool
	remove bool
	putURL bool
}

func (d *fakeDriver) Config() driver.Config {
	return driver.Config{Name: d.name, NoCache: true}
}
func (d *fakeDriver) GetAddition() driver.Additional { return &struct{}{} }
func (d *fakeDriver) Init(ctx context.Context) error { return nil }
func (d *fakeDriver) Drop(ctx context.Context) error { return nil }
func (d *fakeDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	return nil, nil
}
func (d *fakeDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return nil, nil
}

// conditionally-present optional interfaces via embedding is awkward; instead we
// use distinct concrete types so the type assertions in DeriveStorageCapabilities
// see exactly the intended set.

type fakeMkdirRemove struct{ fakeDriver }

func (d *fakeMkdirRemove) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	return nil
}
func (d *fakeMkdirRemove) Remove(ctx context.Context, obj model.Obj) error { return nil }

func TestDeriveStorageCapabilities(t *testing.T) {
	d := &fakeMkdirRemove{fakeDriver{name: "FakeMR"}}
	caps := DeriveStorageCapabilities(d)
	got := map[ext.Capability]bool{}
	for _, c := range caps {
		got[c] = true
	}
	if !got[ext.CapMkdir] || !got[ext.CapRemove] {
		t.Fatalf("expected Mkdir+Remove, got %v", caps)
	}
	if got[ext.CapCopy] || got[ext.CapPut] {
		t.Fatalf("unexpected capability derived: %v", caps)
	}
}

func TestRegisterStorageDriverAndLookup(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterStorageDriver(func() driver.Driver {
		return &fakeMkdirRemove{fakeDriver{name: "FakeMR"}}
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// lookup by name
	ctor, err := r.GetStorageDriver("FakeMR")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ctor == nil {
		t.Fatal("nil constructor")
	}
	// names + info map
	names := r.StorageDriverNames()
	if len(names) != 1 || names[0] != "FakeMR" {
		t.Fatalf("names = %v", names)
	}
	if _, ok := r.StorageDriverInfoMap()["FakeMR"]; !ok {
		t.Fatalf("info map missing FakeMR")
	}
	// entry carries derived capabilities on a StorageDriver-kind, constructor mode
	e, ok := r.Get("FakeMR")
	if !ok || e.Kind != ext.KindStorageDriver || e.Mode != InstanceModeConstructor {
		t.Fatalf("entry = %+v ok=%v", e, ok)
	}
}

func TestEnableDisable(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterStorageDriver(func() driver.Driver {
		return &fakeMkdirRemove{fakeDriver{name: "Toggle"}}
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.Disable("Toggle"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := r.GetStorageDriver("Toggle"); err == nil {
		t.Fatal("disabled driver should not resolve")
	}
	if len(r.ByKind(ext.KindStorageDriver)) != 0 {
		t.Fatal("disabled entry should be excluded from ByKind")
	}
	if err := r.Enable("Toggle"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := r.GetStorageDriver("Toggle"); err != nil {
		t.Fatalf("re-enabled driver should resolve: %v", err)
	}
}

func TestConstructorVsSingletonMode(t *testing.T) {
	r := NewRegistry()
	// constructor via storage driver
	_ = r.RegisterStorageDriver(func() driver.Driver {
		return &fakeMkdirRemove{fakeDriver{name: "Ctor"}}
	})
	// singleton via generic Register
	sing := &Entry{
		Manifest: ext.Manifest{ID: "sing.job", Kinds: []ext.Kind{ext.KindScheduledJob}},
		Kind:     ext.KindScheduledJob,
		Mode:     InstanceModeSingleton,
		enabled:  true,
	}
	if err := r.Register(sing); err != nil {
		t.Fatalf("register singleton: %v", err)
	}
	c, _ := r.Get("Ctor")
	if c.Mode != InstanceModeConstructor {
		t.Fatalf("Ctor mode = %v", c.Mode)
	}
	s, _ := r.Get("sing.job")
	if s.Mode != InstanceModeSingleton {
		t.Fatalf("sing.job mode = %v", s.Mode)
	}
	if got := r.ByKind(ext.KindScheduledJob); len(got) != 1 || got[0].ID() != "sing.job" {
		t.Fatalf("ByKind(ScheduledJob) = %v", got)
	}
}

// TestRegisterClaimsPublicPrefix covers the cross-plugin mutual-exclusion
// table for Contributions.PublicPrefix (design §3 PublicRouter): the first
// registration claims it, ResolvePublicPrefix answers case-insensitively, and
// a Deregister releases the claim so a later registrant can take it.
func TestRegisterClaimsPublicPrefix(t *testing.T) {
	r := NewRegistry()
	e := &Entry{
		Manifest: ext.Manifest{ID: "bundle.share", Kinds: []ext.Kind{ext.KindFeatureBundle},
			Contributions: ext.Contributions{PublicPrefix: "/s"}},
		Kind:    ext.KindFeatureBundle,
		enabled: true,
	}
	if err := r.Register(e); err != nil {
		t.Fatalf("Register: %v", err)
	}
	id, ok := r.ResolvePublicPrefix("s")
	if !ok || id != "bundle.share" {
		t.Fatalf("ResolvePublicPrefix(s) = %q, %v; want bundle.share, true", id, ok)
	}
	// Case-insensitive lookup.
	if id, ok := r.ResolvePublicPrefix("S"); !ok || id != "bundle.share" {
		t.Fatalf("ResolvePublicPrefix(S) = %q, %v; want bundle.share, true", id, ok)
	}

	r.Deregister("bundle.share")
	if _, ok := r.ResolvePublicPrefix("s"); ok {
		t.Fatal("prefix must be released after Deregister")
	}
}

// TestRegisterPublicPrefixConflictRejected covers cross-plugin mutual
// exclusion: a second extension requesting the SAME prefix a live entry
// already claims must be refused, and — critically — must not be registered
// at all (a rejected claim must not leave a half-registered entry behind).
func TestRegisterPublicPrefixConflictRejected(t *testing.T) {
	r := NewRegistry()
	first := &Entry{
		Manifest: ext.Manifest{ID: "bundle.first", Kinds: []ext.Kind{ext.KindFeatureBundle},
			Contributions: ext.Contributions{PublicPrefix: "/s"}},
		Kind:    ext.KindFeatureBundle,
		enabled: true,
	}
	if err := r.Register(first); err != nil {
		t.Fatalf("Register first: %v", err)
	}
	second := &Entry{
		Manifest: ext.Manifest{ID: "bundle.second", Kinds: []ext.Kind{ext.KindFeatureBundle},
			Contributions: ext.Contributions{PublicPrefix: "/s"}},
		Kind:    ext.KindFeatureBundle,
		enabled: true,
	}
	if err := r.Register(second); err == nil {
		t.Fatal("expected conflict error registering a second claimant of the same prefix")
	}
	if _, ok := r.Get("bundle.second"); ok {
		t.Fatal("rejected registrant must not be left registered")
	}
	// The original claim is untouched.
	if id, ok := r.ResolvePublicPrefix("s"); !ok || id != "bundle.first" {
		t.Fatalf("ResolvePublicPrefix(s) = %q, %v; want bundle.first, true", id, ok)
	}
}

// TestRegisterPublicPrefixReservedWordRejected covers the reserved-word half
// of the check (CheckReservedPrefix) reached through RegisterPluginMode: a
// disk plugin requesting a core-owned top-level prefix ("api") must be
// refused at registration, not just format-validated at manifest load.
func TestRegisterPublicPrefixReservedWordRejected(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{
		ID:            "com.example.reserved",
		Kinds:         []ext.Kind{ext.KindFeatureBundle},
		Contributions: ext.Contributions{PublicPrefix: "/api"},
	}
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err == nil {
		t.Fatal("expected reserved-prefix rejection at RegisterPluginMode")
	}
	if _, ok := r.Get(m.ID); ok {
		t.Fatal("rejected registrant must not be left registered")
	}
}

// TestRegisterPluginModeSamePrefixReclaimIsIdempotent covers the
// re-registration path (Update/Rollback/LoadEnabled all Deregister then
// re-register the SAME id): re-claiming its own already-held prefix must
// succeed, not be treated as a conflict with itself.
func TestRegisterPluginModeSamePrefixReclaimIsIdempotent(t *testing.T) {
	r := NewRegistry()
	m := ext.Manifest{
		ID:            "com.example.reclaim",
		Kinds:         []ext.Kind{ext.KindFeatureBundle},
		Contributions: ext.Contributions{PublicPrefix: "/s"},
	}
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err != nil {
		t.Fatalf("first RegisterPluginMode: %v", err)
	}
	// Simulate a restart/update re-registration: Deregister then re-register
	// the exact same manifest/prefix.
	r.Deregister(m.ID)
	if err := r.RegisterPluginMode(m, "unused-binpath", ExecGRPC); err != nil {
		t.Fatalf("re-RegisterPluginMode: %v", err)
	}
	if id, ok := r.ResolvePublicPrefix("s"); !ok || id != m.ID {
		t.Fatalf("ResolvePublicPrefix(s) = %q, %v; want %s, true", id, ok, m.ID)
	}
}

func TestCapabilityMismatchRejected(t *testing.T) {
	r := NewRegistry()
	// declare Copy but only implement Mkdir → must fail loudly at registration
	e := &Entry{
		Manifest: ext.Manifest{
			ID:           "bad.driver",
			Kinds:        []ext.Kind{ext.KindStorageDriver},
			Capabilities: []ext.Capability{ext.CapCopy},
		},
		Kind:        ext.KindStorageDriver,
		Mode:        InstanceModeConstructor,
		implemented: []ext.Capability{ext.CapMkdir},
		enabled:     true,
	}
	err := r.Register(e)
	if err == nil {
		t.Fatal("expected capability-mismatch rejection")
	}
	var cme *CapabilityMismatchError
	if !errors.As(err, &cme) {
		t.Fatalf("expected CapabilityMismatchError, got %T: %v", err, err)
	}
	if _, ok := r.Get("bad.driver"); ok {
		t.Fatal("rejected entry must not be registered")
	}
}
