package hostcap

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/authlimit"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// recordingGuard is a deterministic ProtocolLoginGuard for asserting exactly
// how hostAuth.Login drives the throttle, without relying on settings-derived
// thresholds. blocked forces the lockout branch; fails/resets count the calls.
type recordingGuard struct {
	blocked      bool
	fails        int
	resets       int
	lastFailKey  string
	lastResetKey string
}

func (g *recordingGuard) Blocked(key string) bool { return g.blocked }
func (g *recordingGuard) Fail(key string)         { g.fails++; g.lastFailKey = key }
func (g *recordingGuard) Reset(key string)        { g.resets++; g.lastResetKey = key }

func withGuard(t *testing.T, g ProtocolLoginGuard) {
	t.Helper()
	prev := protocolLoginGuard
	protocolLoginGuard = g
	t.Cleanup(func() { protocolLoginGuard = prev })
}

func newAuth(pluginID string) *hostAuth {
	return &hostAuth{
		grant: Grant{PluginID: pluginID, HostAPIs: []GrantItem{{Name: CapAuth}}},
		prin:  newPrincipals(),
	}
}

// A wrong password records a throttle failure and is denied; a correct password
// resets the counter and mints a principal. The throttle key is scoped to the
// plugin id + username (no client IP exists over the bridge).
func TestLoginThrottleWiring(t *testing.T) {
	u := &model.User{Username: "thr-wire-user"}
	u.SetPassword("right-pass")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	g := &recordingGuard{}
	withGuard(t, g)
	a := newAuth("p.thr.wire")

	if _, err := a.Login(context.Background(), "thr-wire-user", "wrong-pass"); err != ext.ErrPermissionDenied {
		t.Fatalf("wrong password: want ErrPermissionDenied, got %v", err)
	}
	if g.fails != 1 || g.lastFailKey != "p.thr.wire:thr-wire-user" {
		t.Fatalf("wrong password must Fail(pluginID:username), got fails=%d key=%q", g.fails, g.lastFailKey)
	}

	if _, err := a.Login(context.Background(), "thr-wire-user", "right-pass"); err != nil {
		t.Fatalf("right password: %v", err)
	}
	if g.resets != 1 || g.lastResetKey != "p.thr.wire:thr-wire-user" {
		t.Fatalf("right password must Reset(pluginID:username), got resets=%d key=%q", g.resets, g.lastResetKey)
	}
}

// When the guard reports the key locked out, Login is refused BEFORE the
// credential is checked — a correct password does not slip past a lockout, and
// no further Fail is recorded (the attempt never reached credential checking).
func TestLoginBlockedShortCircuits(t *testing.T) {
	u := &model.User{Username: "thr-blocked-user"}
	u.SetPassword("right-pass")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	g := &recordingGuard{blocked: true}
	withGuard(t, g)
	a := newAuth("p.thr.blocked")

	if _, err := a.Login(context.Background(), "thr-blocked-user", "right-pass"); err != ext.ErrPermissionDenied {
		t.Fatalf("blocked key with right password: want ErrPermissionDenied, got %v", err)
	}
	if g.fails != 0 {
		t.Fatalf("a blocked attempt must not reach credential checking / Fail, got fails=%d", g.fails)
	}
}

// The real internal/authlimit throttle actually locks the account out after the
// admin-tunable threshold of failures — end-to-end proof the brute-force ceiling
// applies, not just that the hooks are called.
func TestLoginRealThrottleLocksOut(t *testing.T) {
	u := &model.User{Username: "thr-real-user"}
	u.SetPassword("right-pass")
	if err := op.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Default guard is a real authlimit.NewThrottle(); ensure a clean one so the
	// package singleton's history from other tests can't interfere.
	withGuard(t, authlimit.NewThrottle())
	a := newAuth("p.thr.real")

	// Exhaust the failure budget with wrong passwords.
	for i := 0; i < 5; i++ {
		if _, err := a.Login(context.Background(), "thr-real-user", "wrong"); err != ext.ErrPermissionDenied {
			t.Fatalf("attempt %d: want ErrPermissionDenied, got %v", i+1, err)
		}
	}
	// Now even the CORRECT password is refused: the key is locked out.
	if _, err := a.Login(context.Background(), "thr-real-user", "right-pass"); err != ext.ErrPermissionDenied {
		t.Fatalf("after lockout, correct password must still be denied, got %v", err)
	}
}
