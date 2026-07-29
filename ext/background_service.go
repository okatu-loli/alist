package ext

import "context"

// BackgroundService is a long-running side task the host starts after boot and
// supervises for the process lifetime (design §2.4): an frp tunnel, a sync
// engine. Unlike ScheduledJob (which the host ticks on a cadence), a
// BackgroundService runs continuously — Run blocks until its context is
// cancelled, and the host restarts it with backoff if it returns early or
// panics. Its Status() feeds the frontend status card (a StatusProvider
// contribution polls it via the host).
//
// Everything the service does inside Run — outbound network, fs access — is
// still borrowed through the injected Host and stays Grant-gated + Authorize-
// enforced by the F0 layer. A background service with no user context that
// touches storage does so under the fs:system scope (design §6.1). frp is the
// canonical case: a native-tier service granted the "network" capability.
type BackgroundService interface {
	Extension

	// Run performs the long-running work. It MUST block until ctx is cancelled
	// (host shutdown or a Stop) and return promptly when it fires. Returning an
	// error (or panicking) is isolated by the host supervisor, which logs it and
	// restarts the service after a backoff — one service crashing never affects
	// the others or the host.
	Run(ctx context.Context) error

	// Status reports the service's current state for the frontend status card. It
	// must be cheap and non-blocking (it is polled): read cached counters, do not
	// perform a live probe. State is "running" | "stopped" | "error"; Detail and
	// Metrics are free-form (e.g. an frp tunnel's proxy/traffic counts).
	Status(ctx context.Context) (ServiceStatus, error)
}

// ServiceStatus is the serializable status a BackgroundService reports.
type ServiceStatus struct {
	// State is the coarse lifecycle state: "running" | "stopped" | "error".
	State string `json:"state"`
	// Detail is an optional human-readable line (last error, current phase).
	Detail string `json:"detail,omitempty"`
	// Metrics is optional free-form structured telemetry for the status card
	// (e.g. {"proxies":3,"traffic_bytes":10485760}).
	Metrics map[string]any `json:"metrics,omitempty"`
}

// Service lifecycle state constants for ServiceStatus.State.
const (
	ServiceStateRunning = "running"
	ServiceStateStopped = "stopped"
	ServiceStateError   = "error"
)
