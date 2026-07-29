package labels

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// Group is the fs/list extension field group the label badge column is fetched
// under (matches the FileColumn contribution's FieldGroup). "_"-prefixed per the
// F2 convention.
const Group = "_labels"

// badge is one label rendered in the listing column: id + plain-text name +
// validated #RRGGBB color. name/color are stored validated (op layer) and the
// frontend renders name as plain text, so the value carries no injection risk.
type badge struct {
	ID    uint   `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Groups declares the single field group this decorator serves.
func (b *bundle) Groups() []string { return []string{Group} }

// Decorate returns the label badges for each ref in the "_labels" group. Every
// ref is a user-visible virtual path (the host filters the listing before
// handing refs to a decorator, F2), so no permission check happens here — a ref
// the caller may not see never reaches this method. Each ref is resolved to an
// EXISTING node (never created: decoration is pure-read); refs with no node or no
// binding simply carry no value. The bindings for the whole page are read in one
// batched query and paired against the (small) vocabulary in memory, avoiding an
// N-way join.
func (b *bundle) Decorate(ctx context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	if group != Group || len(refs) == 0 {
		return nil, nil
	}
	// Resolve each ref's virtual path to a node id without creating one, keeping
	// the mapping node -> the ref key(s) that resolved to it (the host keys the
	// column value by ext.RefKey).
	nodeToKeys := map[uint][]string{}
	var nodeIDs []uint
	for _, rf := range refs {
		anchor, err := op.ResolvePathAnchor(utils.FixAndCleanPath(rf.Anchor))
		if err != nil {
			continue // above every mount / unresolvable -> no labels
		}
		nid, found, err := db.GetNodeByChain(anchor.StorageID, anchor.Anchor)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // never anchored -> no labels
		}
		if _, seen := nodeToKeys[nid]; !seen {
			nodeIDs = append(nodeIDs, nid)
		}
		nodeToKeys[nid] = append(nodeToKeys[nid], ext.RefKey(rf))
	}
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	bindings, err := db.BindingsByNodeIDs(nodeIDs)
	if err != nil {
		return nil, err
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	vocab, err := db.ListLabels()
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]model.Label, len(vocab))
	for _, l := range vocab {
		byID[l.ID] = l
	}
	nodeBadges := map[uint][]badge{}
	for _, bd := range bindings {
		l, ok := byID[bd.LabelID]
		if !ok {
			continue // binding to a since-deleted label (defensive; delete cascades)
		}
		nodeBadges[bd.NodeID] = append(nodeBadges[bd.NodeID], badge{ID: l.ID, Name: l.Name, Color: l.Color})
	}
	out := make(map[string]any, len(nodeToKeys))
	for nid, keys := range nodeToKeys {
		bs := nodeBadges[nid]
		if len(bs) == 0 {
			continue
		}
		for _, k := range keys {
			out[k] = bs
		}
	}
	return out, nil
}
