package bootstrap

import (
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/labels"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// InitLabels registers the built-in "shared labels" feature bundle into the
// process registry (like InitPasskey for the passkey login method). It only
// records the trusted in-process bundle — its FileDecorator column and the
// vocabulary/apply API read live from the db/op layer per request — so it can run
// at boot regardless of storage readiness. A registration failure is fatal: a
// half-wired built-in must not run.
func InitLabels() {
	if err := labels.Register(extension.DefaultRegistry); err != nil {
		utils.Log.Fatalf("failed to register built-in labels bundle: %+v", err)
	}
}
