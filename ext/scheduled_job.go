package ext

import "context"

// ScheduledJob is a time-driven background task a plugin contributes: share
// expiry cleanup, index compaction, a periodic sync (design §2.5 notes share's
// expiry cleanup is a ScheduledJob — time-driven, not event-driven). The host
// supervises scheduling; the plugin only declares its cadence and does the work.
//
// Run executes under the host's supervision: each invocation is panic-isolated
// and non-overlapping (the host skips a tick if the previous Run of the same job
// is still going), so a slow or crashing job degrades only itself. Any host
// capability the job borrows inside Run stays Grant-gated + Authorize-enforced by
// the F0 host layer; a background job with no user context uses the fs:system
// scope (design §6.1) if it needs storage access.
type ScheduledJob interface {
	Extension

	// Schedule returns the job's cadence as an interval spec the host parses
	// (see ParseSchedule): "@every <dur>" (e.g. "@every 30s", "@every 1h"),
	// or the shorthands "@hourly"/"@daily"/"@weekly". An unparseable or
	// non-positive schedule disables the job (the host logs and skips it) rather
	// than defaulting to a surprise cadence.
	Schedule() string

	// Run performs one execution. Returning an error is logged by the host; it
	// does not stop future ticks. Run should honor ctx cancellation (host
	// shutdown) and return promptly when it fires.
	Run(ctx context.Context) error
}
