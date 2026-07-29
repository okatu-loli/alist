package handles

import (
	"github.com/alist-org/alist/v3/internal/authlimit"
)

// The per-IP brute-force policy now lives in internal/authlimit so a lower layer
// that cannot import server/handles (the plugin capability layer, which funnels
// protocol logins through the same policy) can reuse it. These aliases keep the
// existing callers (auth.go / authn.go / auth_refresh.go) and their tests
// unchanged: every unauthenticated credential endpoint still drives its own
// instance through the same three methods, counters kept separate.
type loginThrottle = authlimit.Throttle

func newLoginThrottle() *loginThrottle { return authlimit.NewThrottle() }

// defaultTimes / defaultDuration are re-exported here for the existing
// server/handles tests that assert the fallback thresholds.
var (
	defaultDuration = authlimit.DefaultDuration
	defaultTimes    = authlimit.DefaultTimes
)
