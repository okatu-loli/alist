package agent

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/AlistGo/alist-agent/proto"
)

// Agent-side log ring buffer + logrus hook (design SP-G1: admin observability).
//
// SECURITY — logs now EGRESS to the server. This buffer captures whatever the
// agent logs and, once wired, its contents leave the agent: the server pulls
// them over the authenticated reverse channel via the agent.log.tail method
// (see capproto.go / ReverseAgent.handleLogTail). This changes NOTHING about
// what is logged — it only makes existing log lines fetchable — so the
// invariant that keeps it safe is upstream: NOTHING SECRET may ever be passed
// to utils.Log in the first place. SP-F already established this (the pairing
// token, the ed25519 identity private key, the derived channel session key, and
// capability tokens are never logged — only ids, fingerprints, hostnames, and
// error strings scrubbed of secrets are). Any future log call must preserve
// that: a secret in a log message would now be readable by the server operator
// through this ring, so treat "don't log secrets" as a hard rule, not a style
// preference.

const (
	// DefaultLogRingCapacity is how many of the most-recent log entries the
	// agent keeps addressable for the server to tail. 500 lines is a useful
	// window for diagnosing a stuck/failed task without holding meaningful
	// memory (each entry is a short string + a few scalars).
	DefaultLogRingCapacity = 500
)

// LogEntry (the per-entry wire shape) now lives in proto.LogEntry
// (internal/agent/proto/wire.go, moved there in the W3 proto-split — design
// analysis §6 W3 — because it is ALSO the shape the agent.log.tail channel
// method returns and therefore the G2 frontend contract, shared verbatim
// between agent and server). LogRing below is the agent-side runtime buffer
// that stores and serves proto.LogEntry values; it stays here since it is
// pure agent-side state, never constructed or interpreted by the server.

// LogRing is a bounded, thread-safe ring buffer of the most recent
// proto.LogEntry values, each stamped with a process-monotonic Seq (1,2,3,…)
// so a server can tail incrementally: "give me everything with seq > my last
// cursor". When it is full the oldest entry is evicted to make room; the
// sequence never resets, so a cursor stays meaningful across evictions (a
// caller whose cursor points at an already-evicted entry simply gets the
// oldest still-buffered entry onward — never a duplicate, never a gap within
// what is buffered).
type LogRing struct {
	mu  sync.Mutex
	buf []proto.LogEntry // chronological (oldest first); len <= cap
	cap int
	seq int64 // last assigned seq
}

// NewLogRing builds a ring holding at most capacity entries (a non-positive
// capacity is clamped to 1 so the buffer is always usable).
func NewLogRing(capacity int) *LogRing {
	if capacity <= 0 {
		capacity = 1
	}
	return &LogRing{cap: capacity, buf: make([]proto.LogEntry, 0, capacity)}
}

// Append records one entry, assigning it the next monotonic Seq and evicting
// the oldest entry if the buffer is already at capacity. Returns the stored
// entry (with its assigned Seq). Thread-safe.
func (r *LogRing) Append(ts time.Time, level, msg string) proto.LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e := proto.LogEntry{Seq: r.seq, TS: ts, Level: level, Msg: msg}
	if len(r.buf) >= r.cap {
		// Full: shift everything down one and overwrite the tail, keeping the
		// backing array a fixed size (no unbounded growth).
		copy(r.buf, r.buf[1:])
		r.buf[len(r.buf)-1] = e
	} else {
		r.buf = append(r.buf, e)
	}
	return e
}

// Since returns up to max buffered entries whose Seq is strictly greater than
// since, oldest first, plus next: the cursor the caller should pass as since on
// its following poll. next is the Seq of the last returned entry, or — when
// nothing newer is buffered — since unchanged (so a quiet agent keeps the
// caller's cursor put rather than rewinding it). A non-positive max is treated
// as "the whole buffer".
func (r *LogRing) Since(since int64, max int) (entries []proto.LogEntry, next int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if max <= 0 {
		max = r.cap
	}
	next = since
	for _, e := range r.buf {
		if e.Seq <= since {
			continue
		}
		entries = append(entries, e)
		next = e.Seq
		if len(entries) >= max {
			break
		}
	}
	return entries, next
}

// LastSeq reports the highest Seq assigned so far (0 before anything is
// appended). Mainly for tests/diagnostics.
func (r *LogRing) LastSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// ringHook is a logrus.Hook that appends every fired log entry to a LogRing.
// It captures only the level, the rendered message, and the entry time — never
// the structured Fields map — keeping the ring's contents to exactly what a log
// line's text already is (see the security note above: nothing secret belongs
// in that text).
type ringHook struct{ ring *LogRing }

func (h ringHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h ringHook) Fire(e *logrus.Entry) error {
	h.ring.Append(e.Time, e.Level.String(), e.Message)
	return nil
}

// AttachLogRing installs a ring-buffer hook on logger and returns the ring, so
// every subsequent log line the agent emits through that logger is captured for
// the server to tail. Call once at agent startup (reverse `serve` mode) against
// utils.Log.
func AttachLogRing(logger *logrus.Logger, capacity int) *LogRing {
	ring := NewLogRing(capacity)
	logger.AddHook(ringHook{ring: ring})
	return ring
}
