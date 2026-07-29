package drivers

import (
	// v4 core skeleton: only the local driver is kept for testing.
	// All other drivers were removed in the clean-break rebuild.
	//
	// 123_open is intentionally NOT blank-imported here: it ships only as an
	// installable gRPC plugin (tools/123open-plugin), not as a built-in. The
	// driver package itself is untouched (drivers/123_open/*.go, including
	// its own driver_test.go) — this just stops registering it in-process.
	_ "github.com/alist-org/alist/v3/drivers/alias"
	_ "github.com/alist-org/alist/v3/drivers/crypt"
	_ "github.com/alist-org/alist/v3/drivers/local"
	_ "github.com/alist-org/alist/v3/drivers/url_tree"
)

// All do nothing,just for import
// same as _ import
func All() {

}
