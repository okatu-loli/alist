package extension

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"
	log "github.com/sirupsen/logrus"
)

// ParseSchedule turns a ScheduledJob.Schedule() spec into a tick interval. The
// supported grammar is deliberately small and dependency-free (no cron library
// is vendored): the "@every <dur>" form (Go duration syntax) plus the
// "@hourly"/"@daily"/"@weekly" shorthands. Full 5-field cron expressions are a
// documented seam — add a cron parser (e.g. robfig/cron) when a plugin needs
// calendar scheduling ("run at 03:00 on the 1st"). A non-positive or
// unparseable spec is an error; the scheduler treats that as "job disabled"
// rather than inventing a cadence.
func ParseSchedule(spec string) (time.Duration, error) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "@hourly":
		return time.Hour, nil
	case "@daily":
		return 24 * time.Hour, nil
	case "@weekly":
		return 7 * 24 * time.Hour, nil
	}
	if rest, ok := strings.CutPrefix(spec, "@every "); ok {
		d, err := time.ParseDuration(strings.TrimSpace(rest))
		if err != nil {
			return 0, fmt.Errorf("scheduler: bad @every duration %q: %w", rest, err)
		}
		if d <= 0 {
			return 0, fmt.Errorf("scheduler: non-positive interval %q", spec)
		}
		return d, nil
	}
	return 0, fmt.Errorf("scheduler: unsupported schedule %q (use @every <dur>, @hourly, @daily, @weekly)", spec)
}

// scheduledJobFactory is the gRPC-dispatch seam for ScheduledJob (mirrors
// eventHookFactory). nil until a gRPC bridge lands; in-process instances are the
// tested F1 path.
var scheduledJobFactory func(e *Entry) (ext.ScheduledJob, func(), error)

// SetScheduledJobFactory installs the gRPC ScheduledJob provider.
func SetScheduledJobFactory(fn func(e *Entry) (ext.ScheduledJob, func(), error)) {
	scheduledJobFactory = fn
}

var (
	scheduledJobMu        sync.RWMutex
	scheduledJobInstances = map[string]ext.ScheduledJob{}
)

// RegisterScheduledJobInstance registers an in-process ext.ScheduledJob under id
// (a built-in or a test double). A nil value deregisters.
func RegisterScheduledJobInstance(id string, j ext.ScheduledJob) {
	scheduledJobMu.Lock()
	defer scheduledJobMu.Unlock()
	if j == nil {
		delete(scheduledJobInstances, id)
		return
	}
	scheduledJobInstances[id] = j
}

func resolveScheduledJob(e *Entry) (ext.ScheduledJob, func()) {
	scheduledJobMu.RLock()
	if j, ok := scheduledJobInstances[e.Manifest.ID]; ok {
		scheduledJobMu.RUnlock()
		return j, func() {}
	}
	scheduledJobMu.RUnlock()
	if e.execMode == ExecGRPC && e.binPath != "" && scheduledJobFactory != nil {
		if j, release, err := scheduledJobFactory(e); err == nil {
			return j, release
		}
	}
	return nil, func() {}
}

// Scheduler runs the enabled Kind=ScheduledJob plugins on their declared
// cadence. It owns one goroutine per job: each runs a ticker and calls Run
// synchronously, so a job's invocations never overlap (a Run that outlasts its
// interval simply makes the next tick coalesce — time.Ticker drops, not queues,
// missed ticks) and a panic/error in one job is isolated from every other.
//
// Running scheduled code is the job plugin's declared purpose, so dispatch is
// gated on enablement alone; anything the job then borrows from the host is
// Grant-gated + Authorize-enforced by the F0 layer. A job with no user context
// that touches storage does so under the fs:system scope (design §6.1), checked
// there.
type Scheduler struct {
	r  *Registry
	wg sync.WaitGroup
}

// NewScheduler builds a scheduler over r.
func NewScheduler(r *Registry) *Scheduler { return &Scheduler{r: r} }

// Start launches a supervising goroutine per enabled ScheduledJob. It returns
// immediately; ctx cancellation stops every job (call Wait to join). A job with
// an invalid schedule is logged and skipped.
func (s *Scheduler) Start(ctx context.Context) {
	for _, e := range s.r.ByKind(ext.KindScheduledJob) {
		job, release := resolveScheduledJob(e)
		if job == nil {
			continue
		}
		d, err := ParseSchedule(job.Schedule())
		if err != nil {
			log.Warnf("extension: ScheduledJob %q disabled: %v", e.Manifest.ID, err)
			release()
			continue
		}
		s.wg.Add(1)
		go s.supervise(ctx, e.Manifest.ID, job, d, release)
	}
}

// Wait blocks until every job goroutine has exited (after ctx is cancelled).
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) supervise(ctx context.Context, id string, job ext.ScheduledJob, d time.Duration, release func()) {
	defer s.wg.Done()
	defer release()
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runJobOnce(ctx, id, job)
		}
	}
}

// runJobOnce invokes Run with panic + error isolation.
func runJobOnce(ctx context.Context, id string, job ext.ScheduledJob) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("extension: ScheduledJob %q panicked: %v", id, r)
		}
	}()
	if err := job.Run(ctx); err != nil {
		log.Warnf("extension: ScheduledJob %q run error: %v", id, err)
	}
}

// DefaultScheduler is the process scheduler; boot starts it over DefaultRegistry.
var DefaultScheduler = NewScheduler(DefaultRegistry)
