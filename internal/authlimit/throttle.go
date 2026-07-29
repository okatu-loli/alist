// Package authlimit holds the shared per-key brute-force throttle policy for
// every credential-checking entry point. It was extracted from
// server/handles/login_throttle.go so a lower layer that cannot import
// server/handles (the plugin capability layer internal/extension/hostcap, which
// funnels protocol logins through the same policy) can still reuse the exact
// same thresholds and counter mechanics rather than growing a second,
// divergent limiter.
//
// The key is caller-defined: the web login path keys on client IP; the protocol
// (plugin) login path — which has no client IP over the reverse gRPC bridge —
// keys on pluginID+":"+username. Each entry point owns its own Throttle instance
// so the counters stay separate (a refresh flood must not lock out logins, a
// protocol brute-force must not lock out the web UI, and vice versa) while the
// logic and admin-tunable thresholds are shared (DRY).
package authlimit

import (
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
)

// Throttle wraps credential checking; it never changes how a password/token is
// verified. A key that accumulates MaxAttempts() failures inside the window is
// refused until LockoutDuration() elapses from its last attempt; a success
// resets its counter.
type Throttle struct {
	cache cache.ICache[int]
}

// NewThrottle builds a Throttle with its own private counter store.
func NewThrottle() *Throttle {
	return &Throttle{cache: cache.NewMemCache[int]()}
}

// DefaultTimes / DefaultDuration are the fallback thresholds used when the
// corresponding settings are unset or non-positive. Kept at the historical
// 5 attempts / 5 minutes so the default behavior is lenient enough never to
// trip a user typing a wrong password a few times.
var (
	DefaultDuration = time.Minute * 5
	DefaultTimes    = 5
)

// MaxAttempts is the settings-driven failure threshold (conf.LoginMaxAttempts),
// falling back to DefaultTimes when unset or non-positive.
func MaxAttempts() int {
	n := setting.GetInt(conf.LoginMaxAttempts, DefaultTimes)
	if n <= 0 {
		return DefaultTimes
	}
	return n
}

// LockoutDuration is the settings-driven lockout window
// (conf.LoginLockoutDuration, in seconds), falling back to DefaultDuration when
// unset or non-positive.
func LockoutDuration() time.Duration {
	secs := setting.GetInt(conf.LoginLockoutDuration, int(DefaultDuration/time.Second))
	if secs <= 0 {
		return DefaultDuration
	}
	return time.Duration(secs) * time.Second
}

// Blocked reports whether key has reached the failure threshold and is currently
// locked out. On a hit it refreshes the lockout TTL so a persistent attacker
// stays locked out for the full window measured from its LAST attempt, not its
// first.
func (t *Throttle) Blocked(key string) bool {
	count, ok := t.cache.Get(key)
	if ok && count >= MaxAttempts() {
		t.cache.Expire(key, LockoutDuration())
		return true
	}
	return false
}

// Fail records one failed attempt from key.
func (t *Throttle) Fail(key string) {
	count, _ := t.cache.Get(key)
	t.cache.Set(key, count+1)
}

// Reset clears key's counter after a successful authentication.
func (t *Throttle) Reset(key string) {
	t.cache.Del(key)
}
