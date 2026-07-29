package ext

import "context"

// FileDecorator supplies EXTENSION FIELD GROUPS for the file listing (design
// §2.6): the data behind a FileColumn contribution — label badges, share
// markers. It is the backend of the "field groups, fetched on demand" protocol:
// the base listing (name/size/time/type) always returns, but a decorator's
// group is computed only when a client actually asks for that column
// (fs/list?fields=_labels), and each group runs under a per-group time budget so
// a slow decorator is marked pending and backfilled rather than blocking the
// listing.
//
// A decorator must be pure-read and fast (typically a single indexed query
// against its own Store table): the host calls it in parallel across decorators
// under a ~200ms per-group budget. The host only ever passes refs the requesting
// user may already see (the caller filters the listing first), so a decorator
// cannot be used to probe objects outside the user's view.
type FileDecorator interface {
	Extension

	// Groups names the field groups this decorator provides (each "_"-prefixed,
	// e.g. "_labels"), matching a FileColumn contribution's FieldGroup. The host
	// routes a requested group to the decorator(s) that declare it.
	Groups() []string

	// Decorate returns the field values for one group over a batch of refs. The
	// result maps a ref's serialized key (RefKey) to the arbitrary field value the
	// column renders (a badge list, a boolean, …). A ref absent from the map simply
	// has no value for this group. Decorate must honor ctx: the host cancels it at
	// the per-group budget and marks the group pending.
	Decorate(ctx context.Context, group string, refs []ObjRef) (map[string]any, error)
}

// RefKey is the canonical serialization of an ObjRef used as the map key in a
// FileDecorator result and in the fs/fields backfill response. It is the shared
// contract both the host and a decorator key by. A borrowed-fs ref (empty
// StorageID, virtual path in Anchor) keys by its path directly; a
// storage-anchored ref prefixes the storage id so the two ref worlds never
// collide.
func RefKey(ref ObjRef) string {
	if ref.StorageID == "" {
		return ref.Anchor
	}
	return ref.StorageID + "\x00" + ref.Anchor
}
