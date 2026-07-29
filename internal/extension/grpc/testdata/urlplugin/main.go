// Command urlplugin serves the synthetic URL-type ext-native driver as a
// go-plugin gRPC plugin, covering the ByteSource==url branch of the 9.2 protocol
// parity.
package main

import (
	"github.com/AlistGo/alist-ext/grpcplugin"
	"github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urldriver"
)

func main() {
	grpcplugin.Serve(urldriver.New())
}
