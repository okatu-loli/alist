package host

import (
	"math"
	"testing"
	"time"
)

// LOW-1: a ttl_ms that would overflow int64 nanoseconds must be clamped to a
// bounded, still-EXPIRING duration — never wrap negative into "never expires".
func TestSignTTLClampsOverflow(t *testing.T) {
	cases := []struct {
		name  string
		ttlMs int64
		want  time.Duration
	}{
		{"zero is explicit non-expiring", 0, 0},
		{"negative is explicit non-expiring", -1, 0},
		{"small value passes through", 1500, 1500 * time.Millisecond},
		{"at cap passes through", int64(maxSignTTL / time.Millisecond), maxSignTTL},
		{"just over cap clamps", int64(maxSignTTL/time.Millisecond) + 1, maxSignTTL},
		{"overflowing value clamps (must not go negative)", math.MaxInt64, maxSignTTL},
		{"another overflowing value clamps", math.MaxInt64 / 1000, maxSignTTL},
	}
	for _, c := range cases {
		got := signTTL(c.ttlMs)
		if got != c.want {
			t.Errorf("%s: signTTL(%d) = %v, want %v", c.name, c.ttlMs, got, c.want)
		}
		// The core invariant: a positive ttl_ms never becomes a non-positive
		// (never-expiring) duration.
		if c.ttlMs > 0 && got <= 0 {
			t.Errorf("%s: positive ttl_ms %d produced non-expiring duration %v", c.name, c.ttlMs, got)
		}
	}
}
