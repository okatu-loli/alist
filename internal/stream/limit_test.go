package stream

import (
	"context"
	"io"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestBurstLimiterAcceptsWritesLargerThanBurst is the whole reason burstLimiter
// exists: a single WaitN whose n exceeds the burst must succeed by draining in
// chunks, whereas a bare rate.Limiter rejects it. The bare-limiter assertion is
// a precondition guard so the test can't silently become vacuous.
func TestBurstLimiterAcceptsWritesLargerThanBurst(t *testing.T) {
	const burst = 1000
	// A huge rate keeps the test fast; only the burst/n relationship is under test.
	lim := NewBurstLimiter(rate.Limit(1<<30), burst)
	if err := lim.WaitN(context.Background(), 100*burst); err != nil {
		t.Fatalf("burst limiter must accept n > burst without error: %v", err)
	}

	bare := rate.NewLimiter(rate.Limit(1<<30), burst)
	if err := bare.WaitN(context.Background(), 100*burst); err == nil {
		t.Fatal("precondition: a bare rate.Limiter must reject n > burst, else this test is vacuous")
	}
}

// TestBurstLimiterThrottlesBeyondBurst asserts the limiter actually slows a
// transfer that outruns its refill rate. With 10_000 tokens/s and a 1000-token
// burst, moving 2000 tokens spends the full burst immediately then waits ~100ms
// for the remaining 1000 to refill. Only a lower bound is asserted (the token
// math cannot go FASTER than this), keeping the test non-flaky on slow CI.
func TestBurstLimiterThrottlesBeyondBurst(t *testing.T) {
	lim := NewBurstLimiter(rate.Limit(10_000), 1000)
	start := time.Now()
	if err := lim.WaitN(context.Background(), 2000); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("expected throttling to take ~100ms for the over-burst tokens, took %v", elapsed)
	}
}

// TestBurstLimiterInfIsUnlimited: a negative/unset limit maps to rate.Inf
// (see bootstrap.streamFilterNegative), which must never throttle regardless of
// how many tokens are requested.
func TestBurstLimiterInfIsUnlimited(t *testing.T) {
	lim := NewBurstLimiter(rate.Inf, 0)
	start := time.Now()
	if err := lim.WaitN(context.Background(), 100_000_000); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("an unlimited limiter must not throttle, took %v", elapsed)
	}
}

// TestRateLimitWriterHonorsLimiter proves the download-path wrapper actually
// blocks on its limiter: two 1000-byte writes against a 10_000 B/s, 1000-burst
// limiter spend the burst on the first write and wait ~100ms on the second.
func TestRateLimitWriterHonorsLimiter(t *testing.T) {
	w := &RateLimitWriter{
		Writer:  io.Discard,
		Limiter: NewBurstLimiter(rate.Limit(10_000), 1000),
		Ctx:     context.Background(),
	}
	buf := make([]byte, 1000)
	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := w.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("rate-limited writer should throttle the second write, took %v", elapsed)
	}
}

// TestRateLimitWriterUnlimitedDoesNotThrottle: with an Inf limiter the wrapper
// is a pass-through and must not add latency — the default (unlimited) case.
func TestRateLimitWriterUnlimitedDoesNotThrottle(t *testing.T) {
	w := &RateLimitWriter{
		Writer:  io.Discard,
		Limiter: NewBurstLimiter(rate.Inf, 0),
		Ctx:     context.Background(),
	}
	buf := make([]byte, 1<<20)
	start := time.Now()
	for i := 0; i < 16; i++ {
		if _, err := w.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("an unlimited writer must not throttle, took %v", elapsed)
	}
}
