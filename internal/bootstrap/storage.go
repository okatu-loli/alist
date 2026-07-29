package bootstrap

import (
	"context"
	"fmt"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

func LoadStorages() {
	storages, err := db.GetEnabledStorages()
	if err != nil {
		utils.Log.Fatal(i18n.Log("log.storage_load_enabled_failed", "error", fmt.Sprintf("%+v", err)))
	}
	go func(storages []model.Storage) {
		for i := range storages {
			err := op.LoadStorage(context.Background(), storages[i])
			if err != nil {
				utils.Log.Error(i18n.Log("log.storage_load_failed",
					"mount_path", storages[i].MountPath, "error", fmt.Sprintf("%+v", err)))
			} else {
				utils.Log.Info(i18n.Log("log.storage_load_success",
					"mount_path", storages[i].MountPath, "driver", storages[i].Driver, "order", storages[i].Order))
			}
		}
		conf.StoragesLoaded = true
	}(storages)
}
