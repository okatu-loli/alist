package main

import (
	"github.com/AlistGo/alist-ext/grpcplugin"
)

// The FRP tunnel packaged as a real out-of-tree BackgroundService plugin: a
// standalone gRPC binary the host dispenses over go-plugin. Unlike the built-in
// internal/frp service (which reads alist settings in-process), this plugin
// process has no settings/db layer — it reads its config from environment
// variables the host injects (see loadConfig).
func main() {
	grpcplugin.ServeBackgroundService(newFrpService())
}
