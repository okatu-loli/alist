package bootstrap

import "github.com/alist-org/alist/v3/internal/op"

// InitObjRelocateHandlers wires node-based relocation (v4 permission stage 1):
// a move/rename repoints a single nodes-table row and every anchored
// permission/meta follows via its derived path. Called once during startup,
// after data.InitData().
func InitObjRelocateHandlers() {
	op.RegisterNodeRelocation()
}
