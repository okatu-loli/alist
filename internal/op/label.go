package op

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// colorRe accepts a six-digit "#RRGGBB" hex color (either case). An empty color
// is allowed separately (validateColor); anything else is rejected so a stored
// color can be dropped straight into a style attribute without opening an
// injection vector (spec §6).
var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validateColor(color string) error {
	if color == "" {
		return nil
	}
	if !colorRe.MatchString(color) {
		return fmt.Errorf("invalid label color %q: must be empty or #RRGGBB hex", color)
	}
	return nil
}

// normalizeLabelName trims surrounding whitespace and rejects an empty result.
// The name is stored and rendered as plain text (never HTML), so no further
// sanitization is needed here.
func normalizeLabelName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("label name must not be empty")
	}
	return name, nil
}

// CreateLabel validates and inserts a new vocabulary entry. Name is trimmed and
// must be unique (case-sensitive, matching the DB unique index); color must be
// empty or #RRGGBB.
func CreateLabel(l *model.Label) error {
	name, err := normalizeLabelName(l.Name)
	if err != nil {
		return err
	}
	l.Name = name
	if err := validateColor(l.Color); err != nil {
		return err
	}
	if _, found, err := db.GetLabelByName(l.Name); err != nil {
		return err
	} else if found {
		return fmt.Errorf("label name %q already exists", l.Name)
	}
	return db.CreateLabel(l)
}

// UpdateLabel validates and saves an edit. The name-uniqueness check excludes the
// row itself, so renaming a label to its current name (color-only edits) is fine.
func UpdateLabel(l *model.Label) error {
	name, err := normalizeLabelName(l.Name)
	if err != nil {
		return err
	}
	l.Name = name
	if err := validateColor(l.Color); err != nil {
		return err
	}
	if existing, found, err := db.GetLabelByName(l.Name); err != nil {
		return err
	} else if found && existing.ID != l.ID {
		return fmt.Errorf("label name %q already exists", l.Name)
	}
	return db.UpdateLabel(l)
}

// DeleteLabel drops the label and cascades its bindings (db-level transaction).
func DeleteLabel(id uint) error {
	return db.DeleteLabel(id)
}

// ListLabels returns the whole vocabulary.
func ListLabels() ([]model.Label, error) {
	return db.ListLabels()
}

// GetLabel returns one vocabulary entry by id.
func GetLabel(id uint) (*model.Label, error) {
	return db.GetLabelByID(id)
}

// ApplyLabel binds labelID onto the object at vpath. The path is cleaned and
// resolved to its (storage, relpath) anchor, whose node chain is created on
// demand (EnsureNodeChain) — the same anchoring permissions and metas use — then
// the binding is written idempotently. Callers gate this on write permission
// (op.Authorize ActWrite) at the HTTP layer; op itself only anchors and binds.
func ApplyLabel(ctx context.Context, vpath string, labelID uint) error {
	ref, err := ResolvePathAnchor(utils.FixAndCleanPath(vpath))
	if err != nil {
		return err
	}
	nid, err := db.EnsureNodeChain(ref.StorageID, ref.Anchor)
	if err != nil {
		return err
	}
	return db.BindLabel(labelID, nid)
}

// RemoveLabel unbinds labelID from the object at vpath. When no node has ever
// been anchored at vpath there is nothing bound, so it is a no-op — removing a
// label must never fabricate a node.
func RemoveLabel(ctx context.Context, vpath string, labelID uint) error {
	nid, found, err := resolveNodeForPath(vpath)
	if err != nil || !found {
		return err
	}
	return db.UnbindLabel(labelID, nid)
}

// LabelsOf returns the labels attached to the object at vpath. An unanchored path
// (no node) has no labels — an empty result, not an error.
func LabelsOf(vpath string) ([]model.Label, error) {
	nid, found, err := resolveNodeForPath(vpath)
	if err != nil || !found {
		return nil, err
	}
	return db.LabelsByNode(nid)
}

// resolveNodeForPath maps a virtual path to its node id without creating one:
// resolve the (storage, relpath) anchor, then look the node chain up. found is
// false when the path is above every mount is handled by the resolve error, or
// when no node exists yet at that anchor.
func resolveNodeForPath(vpath string) (nodeID uint, found bool, err error) {
	ref, err := ResolvePathAnchor(utils.FixAndCleanPath(vpath))
	if err != nil {
		return 0, false, err
	}
	return db.GetNodeByChain(ref.StorageID, ref.Anchor)
}
