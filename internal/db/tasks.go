package db

import (
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

func GetTaskDataByType(type_s string) (*model.TaskItem, error) {
	task := model.TaskItem{Key: type_s}
	if err := db.Where(task).First(&task).Error; err != nil {
		return nil, errors.Wrapf(err, "failed find task")
	}
	return &task, nil
}

// UpdateTaskData upserts the persisted blob for a task type. The row may not
// exist yet (e.g. first ever persist after enabling TaskPersistant, or a task
// type that InitialTasks() never seeded), so fall back to creating it when the
// update touches zero rows instead of silently discarding the write.
func UpdateTaskData(t *model.TaskItem) error {
	result := db.Model(&model.TaskItem{}).Where("key = ?", t.Key).Update("persist_data", t.PersistData)
	if result.Error != nil {
		return errors.WithStack(result.Error)
	}
	if result.RowsAffected == 0 {
		return errors.WithStack(db.Create(t).Error)
	}
	return nil
}

func CreateTaskData(t *model.TaskItem) error {
	return errors.WithStack(db.Create(t).Error)
}

// GetTaskDataFunc returns the persist-read function passed to
// tache.WithPersistFunction. It must always be non-nil when persistence is
// enabled: tache.NewManager only recovers/persists tasks when both the read
// and write functions are non-nil (see manager.go), so if this returned nil
// whenever no row exists yet (fresh install, task type never seeded),
// persistence would silently stay disabled forever. Instead the closure
// queries lazily and treats "no row yet" / "not found" as an empty task list.
func GetTaskDataFunc(type_s string, enabled bool) func() ([]byte, error) {
	if !enabled {
		return nil
	}
	return func() ([]byte, error) {
		task, err := GetTaskDataByType(type_s)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return []byte("[]"), nil
			}
			return nil, err
		}
		if task.PersistData == "" {
			return []byte("[]"), nil
		}
		return []byte(task.PersistData), nil
	}
}

func UpdateTaskDataFunc(type_s string, enabled bool) func([]byte) error {
	if !enabled {
		return nil
	}
	return func(data []byte) error {
		s := string(data)
		if s == "null" || s == "" {
			s = "[]"
		}
		return UpdateTaskData(&model.TaskItem{Key: type_s, PersistData: s})
	}
}
