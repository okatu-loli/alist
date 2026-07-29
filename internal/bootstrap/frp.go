package bootstrap

import (
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/frp"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitFrp registers the built-in frp tunnel BackgroundService into the process
// registry (like InitLabels). It only records the trusted in-process service —
// the service reads live settings inside Run — so it runs at boot regardless of
// storage readiness. The supervisor (StartBackgroundServices) starts it. A
// registration failure is fatal: a half-wired built-in must not run.
func InitFrp() {
	if err := frp.Register(extension.DefaultRegistry); err != nil {
		utils.Log.Fatalf("failed to register built-in frp service: %+v", err)
	}
}
