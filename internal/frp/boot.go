package frp

import (
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/op"
)

// Register installs the built-in frp tunnel BackgroundService into r. It records
// the trusted in-process singleton so the ServiceSupervisor starts it at boot;
// the service reads live settings inside Run, so it can register before storage
// is ready (like InitLabels).
//
// It also wires the hot-reload seam (design §2.4): any setting change triggers
// instance.Reload(), which cancels the in-flight frpc session so Run re-enters
// with fresh config. MVP reloads unconditionally on every setting change — one
// frpc restart is cheap, and it keeps the callback free of frp-key coupling.
func Register(r *extension.Registry) error {
	if err := extension.RegisterBuiltinBackgroundService(r, ServiceID, instance); err != nil {
		return err
	}
	op.RegisterSettingChangingCallback(func() { instance.Reload() })
	return nil
}
