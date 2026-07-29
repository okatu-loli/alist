package db

import (
	"fmt"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// labelBindingsJoin builds "JOIN label_bindings lb ON lb.label_id = labels.id"
// with both table names resolved through the live NamingStrategy, so it honors
// --table-prefix instead of hardcoding the unprefixed names (same shape as
// permission.go's rolePermissionsJoin).
func labelBindingsJoin() string {
	return fmt.Sprintf("JOIN %s lb ON lb.label_id = %s.id",
		db.NamingStrategy.TableName("LabelBinding"), db.NamingStrategy.TableName("Label"))
}

func CreateLabel(l *model.Label) error {
	return errors.WithStack(db.Create(l).Error)
}

func UpdateLabel(l *model.Label) error {
	return errors.WithStack(db.Save(l).Error)
}

// DeleteLabel removes the label and cascades to every binding that references
// it, in one transaction — a label id must never survive on an orphaned binding.
func DeleteLabel(id uint) error {
	return errors.WithStack(db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("label_id = ?", id).Delete(&model.LabelBinding{}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.Label{}, id).Error
	}))
}

// ListLabels returns the whole vocabulary, id-ordered.
func ListLabels() ([]model.Label, error) {
	var ls []model.Label
	if err := db.Order("id").Find(&ls).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return ls, nil
}

func GetLabelByID(id uint) (*model.Label, error) {
	var l model.Label
	if err := db.First(&l, id).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return &l, nil
}

// GetLabelByName looks a label up by its unique name; found is false (nil error)
// when none exists. Backs op's name-uniqueness check.
func GetLabelByName(name string) (l *model.Label, found bool, err error) {
	var found1 model.Label
	e := db.Where("name = ?", name).First(&found1).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, errors.WithStack(e)
	}
	return &found1, true, nil
}

// BindLabel binds labelID onto nodeID. Idempotent: the composite unique index
// (label_id, node_id) turns a duplicate insert into a no-op via OnConflict, so
// re-applying the same label to the same node never errors or duplicates.
func BindLabel(labelID, nodeID uint) error {
	b := model.LabelBinding{LabelID: labelID, NodeID: nodeID}
	return errors.WithStack(db.Clauses(clause.OnConflict{DoNothing: true}).Create(&b).Error)
}

// UnbindLabel removes the (labelID, nodeID) binding; absent binding is a no-op.
func UnbindLabel(labelID, nodeID uint) error {
	return errors.WithStack(db.Where("label_id = ? AND node_id = ?", labelID, nodeID).
		Delete(&model.LabelBinding{}).Error)
}

// LabelsByNode returns every label bound to nodeID, joined through the bindings
// table. Empty (nil) when the node has none.
func LabelsByNode(nodeID uint) ([]model.Label, error) {
	var ls []model.Label
	err := db.Model(&model.Label{}).
		Joins(labelBindingsJoin()).
		Where("lb.node_id = ?", nodeID).
		Find(&ls).Error
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return ls, nil
}

// BindingsByNodeIDs returns the raw bindings for a set of nodes in one query —
// the batch the list decorator uses: it pairs these with the (small) vocabulary
// from ListLabels to attach {id,name,color} per node without an N-way join.
// Empty input is an empty result.
func BindingsByNodeIDs(nodeIDs []uint) ([]model.LabelBinding, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	var bs []model.LabelBinding
	if err := db.Where("node_id IN ?", nodeIDs).Find(&bs).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return bs, nil
}

// BindingsByLabel returns the node IDs a label is bound to — the source for the
// "files with this label" filter view (each node id resolves to a virtual path
// the caller then permission-filters).
func BindingsByLabel(labelID uint) ([]uint, error) {
	var ids []uint
	if err := db.Model(&model.LabelBinding{}).Where("label_id = ?", labelID).
		Pluck("node_id", &ids).Error; err != nil {
		return nil, errors.WithStack(err)
	}
	return ids, nil
}
