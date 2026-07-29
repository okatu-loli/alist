package ext

import "context"

// Notification is a sink the host fans notices out to (design: NotificationChannel
// kind) — a plugin forwarding host notices to an external channel (email,
// webhook, Telegram, ...). Like EventHook it is a NOTIFICATION sink, not a
// veto: the host has already decided a notice is worth raising by the time
// Notify is called, so a channel cannot block or alter it — only forward or
// drop it (by failing) on its own.
//
// Every Notify runs in failure isolation (the host recovers panics and
// swallows errors, logging them): one channel erroring never affects the
// host or the other channels. A channel that needs to borrow host
// capabilities (render a richer message, look up a display name) does so
// through the injected Host, which remains Grant-gated and
// Authorize-enforced (design §0, §6.3) — the Notice payload itself only
// carries the notice's shape, never object bytes or credentials.
type Notification interface {
	Extension

	// Notify delivers one notice to this channel. Its error/panic is isolated
	// by the host. It should return promptly; heavy delivery (an outbound HTTP
	// call, a retry queue) belongs on the plugin's own queue, not inline in the
	// host's dispatch.
	Notify(ctx context.Context, n Notice) error
}

// NoticeLevel is the severity of a Notice.
type NoticeLevel string

const (
	NoticeInfo  NoticeLevel = "info"
	NoticeWarn  NoticeLevel = "warn"
	NoticeError NoticeLevel = "error"
)

// Notice is the serializable payload delivered to Notification.Notify. It
// carries only the notice's shape: never object bytes (a channel that needs
// more re-reads it through the Grant-gated Host) and never credentials.
type Notice struct {
	// Level is the notice's severity.
	Level NoticeLevel `json:"level"`
	// Title is a short human-readable summary.
	Title string `json:"title"`
	// Body is an optional longer human-readable description.
	Body string `json:"body,omitempty"`
	// Source is the host subsystem that raised the notice, e.g. "task",
	// "storage" (empty when the raiser did not identify itself).
	Source string `json:"source,omitempty"`
	// UnixMilli is the host wall-clock time the notice was raised (0 when
	// unset).
	UnixMilli int64 `json:"unix_milli,omitempty"`
}
