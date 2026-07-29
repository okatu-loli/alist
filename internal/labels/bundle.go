// Package labels is the built-in "shared labels" feature bundle: a composite
// (design §2.5) that decorates the file listing with label badges and declares
// the label column + right-click menu contributions. The vocabulary CRUD and the
// apply/remove/filter API are native server handlers (server/handles/labels.go),
// authorization-gated through op.Authorize; this package owns the extension-point
// wiring (FeatureBundle + FileDecorator) and its boot registration.
package labels

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// BundleID is the built-in labels bundle's registry id (reverse-domain, "builtin"
// namespace like the passkey/SSO built-ins).
const BundleID = "builtin.labels"

// bundle is the single value implementing Extension + FeatureBundle +
// FileDecorator. Its Groups/Decorate methods live in decorator.go.
type bundle struct{}

// instance is the process-wide built-in; it is stateless (reads go straight to
// the core db/op layer).
var instance = &bundle{}

// Manifest declares the trusted built-in bundle and its front-end contributions:
// the label badge column (FileColumn, data source = the "_labels" FileDecorator)
// and the right-click "Labels" menu action.
func (b *bundle) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:         BundleID,
		Name:       map[string]string{"en": "Labels", "zh": "标签"},
		Version:    "builtin",
		CoreAPI:    "builtin",
		Kinds:      []ext.Kind{ext.KindFeatureBundle, ext.KindFileDecorator},
		Provenance: ext.Provenance{Source: "official", License: "builtin"},
		Contributions: ext.Contributions{
			FileColumns: []ext.FileColumn{{
				ID:         "labels",
				Header:     map[string]string{"en": "Labels", "zh": "标签"},
				FieldGroup: Group,
			}},
			MenuActions: []ext.MenuAction{{
				ID:     "labels",
				Label:  map[string]string{"en": "Labels", "zh": "标签"},
				Icon:   "tag",
				Target: "file",
			}},
			// Declare the native host management page so the merged contributions
			// document surfaces it and the frontend generates the nav entry from
			// data (design W1). The /@manage/ prefix marks it a native host page
			// (App.tsx already routes /extensions/labels→LabelsList), not a
			// sandboxed detail-page section.
			SettingsPages: []ext.SettingsPage{{
				ID:    "labels",
				Title: map[string]string{"en": "Labels", "zh": "标签"},
				Route: "/@manage/extensions/labels",
			}},
		},
	}
}

func (b *bundle) Init(ctx context.Context, h ext.Host) error { return nil }
func (b *bundle) Shutdown(ctx context.Context) error         { return nil }

// Bundle declares the host resources the composite needs. Labels persist through
// the core db layer (a native built-in), not the plugin Store, and sign nothing,
// so no host resources are requested.
func (b *bundle) Bundle() ext.BundleSpec { return ext.BundleSpec{} }

// Register installs the built-in labels bundle into r (DefaultRegistry at boot):
// the enabled entry (carrying the FileColumn/MenuAction contributions), the
// in-process instance, and its FileDecorator ("_labels") — so fs/list can fetch
// the label badge column.
func Register(r *extension.Registry) error {
	return extension.RegisterBuiltinFeatureBundle(r, instance)
}
