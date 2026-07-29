// Command localplugin serves the built-in local storage driver as a go-plugin
// gRPC plugin. It is the "same ext driver, over a real subprocess" side of the
// 9.2 parity test: the test builds this binary in TestMain and drives it through
// the host client, comparing against the same bridge run in-process.
//
// It lives under testdata/ so `go build ./...` and `go vet ./...` skip it (the
// parity test builds it explicitly), keeping the extra deps out of the tree
// build.
package main

import (
	"github.com/AlistGo/alist-ext/grpcplugin"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func main() {
	// The local driver's Init queries a setting, so a DB must exist in this
	// subprocess. Mirror the in-memory sqlite setup the bridge/op tests use.
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("localplugin: open db: " + err.Error())
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("localplugin: migrate: " + err.Error())
	}

	ctor, err := extension.DefaultRegistry.GetStorageDriver("Local")
	if err != nil {
		panic("localplugin: local driver not registered: " + err.Error())
	}
	e, _ := extension.DefaultRegistry.Get("Local")
	b := bridge.NewStorageBridge(ctor, e.Manifest)

	grpcplugin.Serve(b)
}
