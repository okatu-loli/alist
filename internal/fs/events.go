package fs

import (
	"context"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/model"
)

// emitFSEvent fans a committed filesystem mutation out to the enabled EventHook
// plugins (design §2.5). It runs AFTER the operation succeeded and never returns
// an error: EventHooks are post-commit notifications, so a hook failure can
// neither fail nor undo the user's operation (extension.DispatchEvent isolates
// panics/errors internally). dst is empty for single-path operations.
//
// The actor is best-effort: the acting *model.User carried on the request
// context (ctxkey.User) when present, otherwise empty for background/system
// operations. Dispatch itself is gated (a plugin needs the "events" grant) and
// any capability a hook then borrows stays Authorize-enforced by the host.
func emitFSEvent(ctx context.Context, typ ext.EventType, path, dst string) {
	var actor string
	if u, ok := ctx.Value(ctxkey.User).(*model.User); ok && u != nil {
		actor = u.Username
	}
	extension.DispatchEventDefault(ctx, ext.Event{
		Type:      typ,
		UnixMilli: time.Now().UnixMilli(),
		Path:      path,
		DstPath:   dst,
		Actor:     actor,
	})
}
