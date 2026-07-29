package agent

import "sync"

// TaskGate is a process-level "one task at a time" guard, shareable across
// every ReverseAgent bound to the SAME underlying reverse-mode agent process.
//
// ReverseAgent already refuses a second dispatch while its own channel has one
// in flight (design R3), but that guard is per-Channel. Channel-stability wave
// 1 also hardens the reverse listener to keep at most one channel alive at a
// time (see channel.Listener's cur swap-and-close), yet the two fixes are
// deliberately layered defense in depth rather than one replacing the other:
// a brief window where an old channel has not yet been torn down (the swap
// races an in-flight dispatch that was already read off the wire) must still
// never let the SAME task run twice. Sharing one TaskGate across every
// ReverseAgent this process constructs closes that gap at the process level,
// independent of which channel a dispatch arrived on.
type TaskGate struct {
	mu       sync.Mutex
	inFlight bool
	activeID string
}

// NewTaskGate returns a ready, idle gate.
func NewTaskGate() *TaskGate { return &TaskGate{} }

// TryStart attempts to claim the gate for taskID. ok is true iff no task was
// already in flight, in which case the caller now owns the gate and must call
// Finish exactly once when done. If ok is false, activeID reports the task
// that already holds the gate (for logging/OnTaskSkipped).
func (g *TaskGate) TryStart(taskID string) (ok bool, activeID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight {
		return false, g.activeID
	}
	g.inFlight = true
	g.activeID = taskID
	return true, ""
}

// Finish releases the gate. Safe to call even if TryStart was never called
// (a no-op), but a caller that successfully claimed the gate must call this
// exactly once when its task ends (success, failure, or cancellation) so a
// later dispatch is not skipped forever.
func (g *TaskGate) Finish() {
	g.mu.Lock()
	g.inFlight = false
	g.activeID = ""
	g.mu.Unlock()
}

// Busy reports whether a task currently holds the gate.
func (g *TaskGate) Busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight
}
