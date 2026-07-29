package model

// SearchNode is one indexed filesystem object, the persisted row behind the
// `builtin.search.database` backend (子项目 A, wave A2). It mirrors v3's
// model.SearchNode: there is deliberately NO path column — the virtual path is
// derived as path.Join(Parent, Name) — so each object is stored once, keyed by
// its parent directory and name. Parent carries a plain index because every
// query filters on a parent-prefix subtree.
//
// No TableName method: v4 relies on gorm's NamingStrategy (pluralize
// "SearchNode" → "search_nodes") plus the configured TablePrefix, so the live
// table is "x_search_nodes" under the default prefix — the same convention as
// every other v4 model (Node → x_nodes, Label → x_labels). The frozen snapshot
// in migration/0019_search_nodes.go carries the matching shape.
type SearchNode struct {
	Parent string `json:"parent" gorm:"index"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Size   int64  `json:"size"`
}
