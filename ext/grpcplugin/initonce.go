package grpcplugin

import (
	"context"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// initState tracks whether an ext.Extension impl's Init has already run once
// in this process, and memoizes that one call's outcome (the error it
// returned, or nil).
type initState struct {
	mu   sync.Mutex
	done bool
	err  error
}

// initGuards maps an impl (by its own identity) to the initState tracking
// whether its Init has already run in this process. Keying by impl itself
// (rather than e.g. its Manifest().ID) relies on every real impl being a
// pointer to the plugin's own implementation type, which is always
// comparable — the same requirement Extension implementations already meet
// implicitly (a value receiver impl could never carry the per-instance state
// most real plugins need anyway).
//
// Why this map exists at all: go-plugin dispenses each Kind (and the
// auto-mounted assist leg — mountAssist) through its own *Plugin/*Server
// pair, and every one of them independently calls impl.Init the first time
// its own InitHost sees a live host. Before multi-Kind single-process
// dispense, that was never a problem in practice: a subprocess only ever
// served one Kind-shaped dispense (assist always got its own dedicated
// subprocess — see pluginproc's assist pool), so InitHost only ever ran once
// per impl per process anyway. Dispensing the same impl under several Kind
// names inside a single subprocess changes that: several independent
// InitHost calls, arriving sequentially or racing each other, can now land
// on the SAME impl in the SAME process. ext.Extension's contract
// (extension.go) promises Init runs "at most once per process, guaranteed
// by the SDK" — this map is what makes that guarantee mechanical rather than
// something every InitHost handler has to reimplement.
var initGuards sync.Map // map[ext.Extension]*initState

func guardFor(impl ext.Extension) *initState {
	v, _ := initGuards.LoadOrStore(impl, &initState{})
	return v.(*initState)
}

// initOnce calls impl.Init(ctx, host) at most once per process for impl: the
// first call to reach here for a given impl builds its host (via buildHost)
// and runs Init, then memoizes the outcome; every later call for the SAME
// impl — a second Kind's InitHost, or a second dispense of the same Kind,
// landing on the identical underlying value — reuses that outcome instead
// of calling Init again, and does not even invoke buildHost (so a repeat
// InitHost never dials a second, immediately-discarded host connection: the
// first Host handed to Init is the one and only Host the impl ever gets).
//
// Concurrent calls for the same impl serialize on initState.mu: the first to
// arrive does the real work, the rest block until it finishes and then all
// observe that single call's result, so two InitHost calls racing each other
// (e.g. two Kind dispenses starting up at once) can never double-Init.
func initOnce(ctx context.Context, impl ext.Extension, buildHost func() (ext.Host, error)) error {
	g := guardFor(impl)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return g.err
	}
	defer func() { g.done = true }()
	h, err := buildHost()
	if err != nil {
		g.err = err
		return err
	}
	g.err = impl.Init(ctx, h)
	return g.err
}
