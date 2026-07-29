package bootstrap

import (
	"context"

	"github.com/alist-org/alist/v3/internal/extension"
)

// StartScheduledJobs starts the extension scheduler over the enabled
// Kind=ScheduledJob plugins (design §2.5: e.g. share expiry cleanup). It MUST
// run after LoadPlugins so the enabled jobs are already registered. Like
// StartTokenGC it is fire-and-forget for the process lifetime — jobs stop when
// the process exits; there is no separate shutdown join point in the server
// command today.
func StartScheduledJobs() {
	extension.DefaultScheduler.Start(context.Background())
}

// StartBackgroundServices starts the enabled Kind=BackgroundService plugins
// (design §2.4: frp tunnel, sync engine) under the supervisor. Like
// StartScheduledJobs it MUST run after LoadPlugins and is fire-and-forget for the
// process lifetime — the supervisor restarts a service that exits/crashes; the
// services stop when the process exits.
func StartBackgroundServices() {
	extension.DefaultServiceSupervisor.Start(context.Background())
}
