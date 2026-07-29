package ext

// ObjRef is the SDK-side projection of the architecture's ResourceRef
// (pillar A). It identifies a resource by storage plus an opaque anchor key,
// replacing v3's path-threading across the boundary (9.0 §7).
type ObjRef struct {
	StorageID string `json:"storage_id"`
	Anchor    string `json:"anchor"` // pillar A: ID-anchored resource key
}

// Obj is the flat, serializable DTO for a filesystem object crossing the
// boundary (9.0 §7). It mirrors v3's model.Obj 8-method interface plus the
// optional Thumb/URL, with wire-friendly scalar fields.
//
// The "…" placeholder in the pre-freeze SDK draft is nailed down here.
type Obj struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"` // unix milliseconds (v3 ModTime)
	Created  int64  `json:"created"`  // unix milliseconds (v3 CreateTime)
	IsDir    bool   `json:"is_dir"`
	ID       string `json:"id"`   // driver-internal id (v3 GetID)
	Path     string `json:"path"` // driver-internal path (v3 GetPath)
	// Hashes MUST be exact whole-file digests of the named algorithm,
	// lowercase hex — never a chunk/partial hash, and never truncated or
	// padded. The host may use these for rapid upload matching and content
	// dedup (content-addressing on the value); a wrong value silently
	// produces wrong content for every consumer that trusts it. Keys are
	// registered hash names ("md5", "sha1", "sha256"); an unrecognized key
	// or a value whose length doesn't match that algorithm's digest width
	// is dropped when crossing the boundary.
	Hashes map[string]string `json:"hashes,omitempty"`
	Thumb  string            `json:"thumb,omitempty"` // v3 Thumb optional interface
	URL    string            `json:"url,omitempty"`   // v3 URL optional interface
}
