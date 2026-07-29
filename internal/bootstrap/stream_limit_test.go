package bootstrap

import (
	"testing"

	"golang.org/x/time/rate"
)

// TestStreamFilterNegative pins the setting→limiter mapping the global caps
// rely on: a negative value (the -1 default) means "unlimited" (rate.Inf, no
// burst), while a positive value is interpreted as KiB/s (×1024) for both the
// rate and the burst.
func TestStreamFilterNegative(t *testing.T) {
	if limit, burst := streamFilterNegative(-1); limit != rate.Inf || burst != 0 {
		t.Fatalf("negative limit must map to unlimited, got limit=%v burst=%d", limit, burst)
	}
	if limit, burst := streamFilterNegative(0); limit != 0 || burst != 0 {
		t.Fatalf("zero limit must map to zero rate, got limit=%v burst=%d", limit, burst)
	}
	if limit, burst := streamFilterNegative(100); limit != rate.Limit(100)*1024 || burst != 100*1024 {
		t.Fatalf("positive limit must convert KiB/s (×1024), got limit=%v burst=%d", limit, burst)
	}
}
