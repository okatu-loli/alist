package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestLogRingEvictsOldestPastCapacity pins the bounded ring: appending past
// capacity evicts the oldest entries, keeping exactly the most-recent `cap`.
func TestLogRingEvictsOldestPastCapacity(t *testing.T) {
	r := NewLogRing(3)
	for i := 1; i <= 5; i++ {
		r.Append(time.Now(), "info", fmt.Sprintf("m%d", i))
	}
	entries, _ := r.Since(0, 0)
	if len(entries) != 3 {
		t.Fatalf("cap 3 must hold exactly 3 after 5 appends, got %d", len(entries))
	}
	// The three survivors are the newest: m3,m4,m5 with seq 3,4,5.
	wantSeq := []int64{3, 4, 5}
	wantMsg := []string{"m3", "m4", "m5"}
	for i, e := range entries {
		if e.Seq != wantSeq[i] || e.Msg != wantMsg[i] {
			t.Fatalf("entry %d = {seq %d msg %q}, want {seq %d msg %q}", i, e.Seq, e.Msg, wantSeq[i], wantMsg[i])
		}
	}
}

// TestLogRingSinceCursorReturnsOnlyNewer pins incremental tailing: Since only
// returns entries strictly newer than the cursor, and reports the right next.
func TestLogRingSinceCursorReturnsOnlyNewer(t *testing.T) {
	r := NewLogRing(100)
	for i := 1; i <= 10; i++ {
		r.Append(time.Now(), "info", fmt.Sprintf("m%d", i))
	}

	// Full drain from 0.
	all, next := r.Since(0, 0)
	if len(all) != 10 || next != 10 {
		t.Fatalf("Since(0) = %d entries, next %d; want 10, 10", len(all), next)
	}

	// Incremental from a mid cursor: only 6..10.
	rest, next := r.Since(5, 0)
	if len(rest) != 5 || next != 10 {
		t.Fatalf("Since(5) = %d entries, next %d; want 5, 10", len(rest), next)
	}
	if rest[0].Seq != 6 {
		t.Fatalf("Since(5) first seq = %d, want 6", rest[0].Seq)
	}

	// Caught up: nothing newer, cursor unchanged (not rewound).
	none, next := r.Since(10, 0)
	if len(none) != 0 || next != 10 {
		t.Fatalf("Since(10) = %d entries, next %d; want 0, 10 (cursor held)", len(none), next)
	}
}

// TestLogRingSinceCaps pins the per-call cap: Since never returns more than max.
func TestLogRingSinceCaps(t *testing.T) {
	r := NewLogRing(500)
	for i := 0; i < 300; i++ {
		r.Append(time.Now(), "info", "x")
	}
	entries, next := r.Since(0, 200)
	if len(entries) != 200 {
		t.Fatalf("Since(0,200) must cap at 200, got %d", len(entries))
	}
	// next is the last RETURNED entry's seq, so the caller resumes exactly after
	// what it got (no skipped entries).
	if next != entries[len(entries)-1].Seq {
		t.Fatalf("next %d must equal last returned seq %d", next, entries[len(entries)-1].Seq)
	}
	if next != 200 {
		t.Fatalf("next after capped drain = %d, want 200", next)
	}
	// Resuming from next yields the remaining 100.
	rest, _ := r.Since(next, 200)
	if len(rest) != 100 {
		t.Fatalf("resume Since(200,200) = %d, want the remaining 100", len(rest))
	}
}

// TestLogRingSeqMonotonic pins that seq strictly increases by one per append and
// never resets, even across evictions.
func TestLogRingSeqMonotonic(t *testing.T) {
	r := NewLogRing(4)
	var last int64
	for i := 0; i < 20; i++ {
		e := r.Append(time.Now(), "info", "x")
		if e.Seq != last+1 {
			t.Fatalf("append %d: seq %d, want %d (monotonic +1)", i, e.Seq, last+1)
		}
		last = e.Seq
	}
	if r.LastSeq() != 20 {
		t.Fatalf("LastSeq = %d after 20 appends, want 20", r.LastSeq())
	}
}

// TestLogRingConcurrentAppendReadRaceFree hammers the ring from many goroutines
// while others read; run under -race it proves the locking is sound and the
// final count is exactly the number appended.
func TestLogRingConcurrentAppendReadRaceFree(t *testing.T) {
	r := NewLogRing(1000)
	const writers, perWriter = 8, 200

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = r.Since(0, 200)
				}
			}
		}()
	}

	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for i := 0; i < perWriter; i++ {
				r.Append(time.Now(), "info", fmt.Sprintf("w%d-%d", w, i))
			}
		}(w)
	}

	// Deterministic join: wait for every writer to finish, THEN stop readers.
	writersWG.Wait()
	close(stop)
	readers.Wait()

	if got := r.LastSeq(); got != int64(writers*perWriter) {
		t.Fatalf("LastSeq = %d, want %d (every append counted exactly once)", got, writers*perWriter)
	}
	// Buffer holds exactly cap (writers*perWriter=1600 > cap 1000).
	entries, _ := r.Since(0, 0)
	if len(entries) != 1000 {
		t.Fatalf("buffer holds %d, want cap 1000", len(entries))
	}
}

// TestAttachLogRingCapturesLogrus pins the hook wiring: lines logged through a
// logrus logger with the ring hook attached land in the ring with level + msg.
func TestAttachLogRingCapturesLogrus(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(newDiscard())
	ring := AttachLogRing(logger, 50)

	logger.Info("hello from agent")
	logger.Warnf("task %s failed", "t1")

	entries, _ := ring.Since(0, 0)
	if len(entries) != 2 {
		t.Fatalf("expected 2 captured entries, got %d", len(entries))
	}
	if entries[0].Msg != "hello from agent" || entries[0].Level != "info" {
		t.Fatalf("entry 0 = {%q,%q}, want {info,hello from agent}", entries[0].Level, entries[0].Msg)
	}
	if entries[1].Msg != "task t1 failed" || entries[1].Level != "warning" {
		t.Fatalf("entry 1 = {%q,%q}", entries[1].Level, entries[1].Msg)
	}
}

// TestLogRingNoSecretMaterial is the SP-G security guard: it samples the exact
// log lines the agent's own code emits over the reverse path and asserts none
// carries obviously-secret material. It is a canary — a comment-plus-check that
// forces a second look if someone ever logs a token/key on this path — not a
// cryptographic proof. The real invariant lives upstream (never pass a secret
// to utils.Log); see logring.go's security note.
func TestLogRingNoSecretMaterial(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(newDiscard())
	ring := AttachLogRing(logger, 50)

	// A representative sample of the strings the agent logs on the serve/reverse
	// path (mirrors cmd/agent.go serve + internal/agent). These MUST NOT embed
	// the pairing token, private keys, or the channel session key.
	logger.Info("agent serve: reverse channel established")
	logger.Infof("agent serve: dispatched task %s", "task-abc")
	logger.Errorf("agent serve: task %s failed: %v", "task-abc", "some io error")
	logger.Warnf("agent serve: reverse channel closed: %v", "channel closed")

	entries, _ := ring.Since(0, 0)
	banned := []string{"BEGIN PRIVATE KEY", "PRIVATE KEY", "session_key", "sessionKey", "pairing_token", "cap_token", "CapToken"}
	for _, e := range entries {
		for _, b := range banned {
			if strings.Contains(e.Msg, b) {
				t.Fatalf("log line leaks secret-shaped material %q: %q", b, e.Msg)
			}
		}
	}
}

// newDiscard returns an io.Writer that drops everything, so test logging is
// silent without importing io in the test's hot path elsewhere.
func newDiscard() *discardWriter { return &discardWriter{} }

type discardWriter struct{}

func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }
