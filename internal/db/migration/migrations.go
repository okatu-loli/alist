// Package migration is the versioned schema migration registry for alist.
// It is a leaf package: it must not import internal/db, internal/conf or
// internal/model — the baseline and every migration carry their own frozen
// struct snapshots so that later model changes cannot silently rewrite
// history.
package migration

import "github.com/go-gormigrate/gormigrate/v2"

var registry []*gormigrate.Migration

// register appends a migration; each migration file calls it from init().
// Files are named NNNN_<slug>.go and IDs are "NNNN_<slug>" so lexical order
// equals apply order.
func register(m *gormigrate.Migration) {
	registry = append(registry, m)
}

// LatestID returns the ID of the newest registered migration (the lexically
// greatest, which by the NNNN_<slug> naming equals the last to apply). It is
// the schema fingerprint the backup package stamps into every archive and
// hard-checks on restore, so a backup taken under one schema can never be
// silently restored into a differently-shaped database. Empty when no
// migration is registered (only possible in a stripped test binary).
func LatestID() string {
	latest := ""
	for _, m := range registry {
		if m.ID > latest {
			latest = m.ID
		}
	}
	return latest
}
