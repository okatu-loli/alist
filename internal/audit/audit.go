// Package audit is the asynchronous audit trail (rollout Step 12): a buffered
// writer that batches security-relevant events into the audit_logs table
// without ever blocking a request, plus retention GC.
//
// It deliberately imports neither gin nor internal/op: callers hand it plain
// values, and request metadata (client IP, protocol) travels via context
// values set by a server middleware — the same decoupling rule Step 8
// established for internal/fs.
package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	log "github.com/sirupsen/logrus"
)

// Context keys the server middleware sets on every API request. Plain string
// keys on purpose: gin.Context.Value resolves string keys from c.Keys, so the
// values survive the gin-context-as-context.Context passing convention the
// handlers already use.
const (
	CtxIPKey       = "audit_client_ip"
	CtxProtocolKey = "audit_protocol"
)

const (
	bufferSize    = 4096
	batchSize     = 128
	flushInterval = 500 * time.Millisecond
	gcInterval    = 24 * time.Hour
	// RetentionDays bounds table growth. A settings-driven knob can layer on
	// later; the writer reads this at each GC sweep.
	RetentionDays = 90
)

var (
	mu      sync.Mutex
	ch      chan model.AuditLog
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
)

// Start launches the background writer + retention GC. Idempotent; Stop
// undoes it (used by tests; production runs it for the process lifetime).
func Start() {
	mu.Lock()
	defer mu.Unlock()
	if ch != nil {
		return
	}
	ch = make(chan model.AuditLog, bufferSize)
	stop = make(chan struct{})
	done = make(chan struct{})
	go run(ch, stop, done)
}

// Stop flushes what is buffered and halts the writer.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if ch == nil {
		return
	}
	close(stop)
	<-done
	ch, stop, done = nil, nil, nil
}

func run(in chan model.AuditLog, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	batch := make([]model.AuditLog, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := db.CreateAuditLogs(batch); err != nil {
			log.WithError(err).Warn("audit: batch insert failed; entries dropped")
		}
		batch = batch[:0]
	}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	gcTicker := time.NewTicker(gcInterval)
	defer gcTicker.Stop()
	gc()
	for {
		select {
		case entry := <-in:
			batch = append(batch, entry)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-gcTicker.C:
			flush()
			gc()
		case <-stopCh:
			// Drain whatever arrived before the stop signal, then flush once.
			for {
				select {
				case entry := <-in:
					batch = append(batch, entry)
				default:
					flush()
					return
				}
			}
		}
	}
}

func gc() {
	n, err := db.PruneAuditLogs(time.Now().AddDate(0, 0, -RetentionDays))
	if err != nil {
		log.WithError(err).Warn("audit: retention GC failed")
	} else if n > 0 {
		log.Debugf("audit: retention GC pruned %d entries", n)
	}
}

// record enqueues without blocking: when the buffer is full the entry is
// dropped and counted — an audit gap is preferable to a stalled request path.
func record(entry model.AuditLog) {
	mu.Lock()
	out := ch
	mu.Unlock()
	if out == nil {
		return // writer not started (CLI subcommands, unit tests)
	}
	entry.Ts = time.Now()
	select {
	case out <- entry:
	default:
		if n := dropped.Add(1); n%1000 == 1 {
			log.Warnf("audit: buffer full, %d entries dropped so far", n)
		}
	}
}

// ip/protocol pulled from the request context (set by the API middleware);
// absent values (background jobs, tests) record as empty.
func fromCtx(ctx context.Context) (ip, protocol string) {
	if ctx == nil {
		return "", ""
	}
	ip, _ = ctx.Value(CtxIPKey).(string)
	protocol, _ = ctx.Value(CtxProtocolKey).(string)
	return ip, protocol
}

// Decision records an authorization decision from op.Authorize.
func Decision(ctx context.Context, u *model.User, action, targetRef string, allowed bool) {
	ip, protocol := fromCtx(ctx)
	entry := model.AuditLog{
		Action:    action,
		TargetRef: targetRef,
		Protocol:  protocol,
		IP:        ip,
		Result:    "deny",
	}
	if allowed {
		entry.Result = "allow"
	}
	if u != nil {
		entry.ActorUID = u.ID
		entry.Actor = u.Username
	}
	record(entry)
}

// Event records a discrete security event (login, logout, session eviction,
// role/permission mutation). actorUID may be 0 for anonymous events.
func Event(ctx context.Context, actorUID uint, actor, action, targetRef string, success bool) {
	ip, protocol := fromCtx(ctx)
	result := "failure"
	if success {
		result = "success"
	}
	record(model.AuditLog{
		ActorUID:  actorUID,
		Actor:     actor,
		Action:    action,
		TargetRef: targetRef,
		Protocol:  protocol,
		IP:        ip,
		Result:    result,
	})
}
