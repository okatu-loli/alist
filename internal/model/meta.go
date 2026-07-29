package model

type Meta struct {
	ID        uint   `json:"id" gorm:"primaryKey"`
	Path      string `json:"path" gorm:"index" binding:"required"`
	Password  string `json:"password"`
	PSub      bool   `json:"p_sub"`
	Write     bool   `json:"write"`
	WSub      bool   `json:"w_sub"`
	Hide      string `json:"hide"`
	HSub      bool   `json:"h_sub"`
	Readme    string `json:"readme"`
	RSub      bool   `json:"r_sub"`
	Header    string `json:"header"`
	HeaderSub bool   `json:"header_sub"`

	// NodeID references the nodes table (v4 permission stage 1): the directory's
	// stable identity, from which the live virtual path is derived (meta
	// resolution matches on that derived path). 0 means unanchored — above every
	// mount point, or a migration row not yet adopted — and then the Path is the
	// truth. Path is always populated (it is the meta's nodeless fallback
	// identity); for a node-anchored row it is a stale display value that the
	// meta index overrides with the derived path. Path carries only a plain index
	// (not unique): a stale stored Path left behind by a move must not block
	// reclaiming that path — meta uniqueness is enforced in op on the LIVE derived
	// path (op.ensureMetaPathFree), never on the stored column.
	NodeID uint `json:"node_id" gorm:"column:node_id;index"`
}
