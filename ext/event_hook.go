package ext

import "context"

// EventHook observes host events — primarily filesystem mutations (move,
// rename, remove, mkdir, upload) — after they succeed (design §2.5: FeatureBundle
// composes EventHook; F1 "事件/定时"). It is a NOTIFICATION sink, not a veto: the
// host fires OnEvent AFTER an operation has already committed, so a hook cannot
// block or undo a user action. This keeps the fs mutation path free of plugin
// latency/failure in its critical section — a misbehaving hook can only fail
// itself.
//
// Every OnEvent runs in failure isolation (the host recovers panics and swallows
// errors, logging them): one hook erroring never affects the originating
// operation nor the other hooks. A hook that needs to borrow host capabilities
// (read the moved file, notify an external service) does so through the injected
// Host, which remains Grant-gated and Authorize-enforced (design §0, §6.3) — the
// Event payload itself only carries the operation's shape (verb + virtual paths
// + actor), never object bytes or credentials.
type EventHook interface {
	Extension

	// OnEvent is called once per host event the plugin is subscribed to. Its
	// error/panic is isolated by the host. It should return promptly; heavy work
	// belongs on a queue the plugin drains, not inline in the host's dispatch.
	OnEvent(ctx context.Context, e Event) error
}

// EventType names a host event. The fs.* family fires after the matching
// filesystem mutation commits.
type EventType string

const (
	// EventFSMkdir fires after a directory is created (Path = the new dir).
	EventFSMkdir EventType = "fs.mkdir"
	// EventFSMove fires after an object is moved (Path = source, DstPath =
	// destination directory).
	EventFSMove EventType = "fs.move"
	// EventFSRename fires after an object is renamed (Path = source, DstPath =
	// the new name).
	EventFSRename EventType = "fs.rename"
	// EventFSRemove fires after an object is removed (Path = the removed object).
	EventFSRemove EventType = "fs.remove"
	// EventFSPut fires after an object is uploaded (Path = destination
	// directory).
	EventFSPut EventType = "fs.put"
)

// Event is the serializable payload delivered to EventHook.OnEvent. It carries
// only the operation's shape: never object bytes (a hook that needs content
// re-reads it through the Grant-gated Host.FS) and never credentials.
type Event struct {
	// Type is the event verb (fs.move, fs.remove, ...).
	Type EventType `json:"type"`
	// UnixMilli is the host wall-clock time the event fired.
	UnixMilli int64 `json:"unix_milli"`
	// Path is the primary virtual path the operation acted on.
	Path string `json:"path"`
	// DstPath is the destination for two-path operations (move/rename); empty
	// otherwise.
	DstPath string `json:"dst_path,omitempty"`
	// Actor is the username of the acting principal, when the host knows it
	// (empty for background/system-initiated operations).
	Actor string `json:"actor,omitempty"`
}
