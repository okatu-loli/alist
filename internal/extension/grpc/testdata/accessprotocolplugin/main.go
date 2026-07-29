// Command accessprotocolplugin serves a synthetic Kind=AccessProtocol
// (TransportHTTP) extension as a go-plugin gRPC plugin (the WebDAV/S3/MCP
// mechanism-proving test double — v4 ships no real HTTP-transport protocol
// plugin yet). It declares a fixed ProtocolInfo and echoes the request
// method/URL/body back in its ServeHTTP response, so a host test can prove
// the full ProtocolInfo/ServeHTTP round trip against a real subprocess.
// Mirrors apirouteplugin (testdata/apirouteplugin) one Kind over.
package main

import (
	"context"
	"net"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type accessProtocolPlugin struct{}

var _ ext.AccessProtocol = (*accessProtocolPlugin)(nil)

func (p *accessProtocolPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testaccessprotocol",
		Name:    map[string]string{"en": "Test Access Protocol"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindAccessProtocol},
	}
}

func (p *accessProtocolPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *accessProtocolPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *accessProtocolPlugin) ProtocolInfo() ext.ProtocolInfo {
	return ext.ProtocolInfo{
		Scheme:       "webdav",
		Transport:    ext.TransportHTTP,
		MountPattern: "/dav",
	}
}

func (p *accessProtocolPlugin) ServeHTTP(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{
		Status: 207,
		Body:   []byte("served:" + req.Method + ":" + req.URL),
	}, nil
}

// HandleConn is never called over gRPC (design §5): this fixture is
// TransportHTTP-only, so it just satisfies the ext.AccessProtocol interface.
func (p *accessProtocolPlugin) HandleConn(ctx context.Context, conn net.Conn) error {
	return ext.NewError(ext.ErrCodeNotImplement, "http transport")
}

func main() {
	grpcplugin.ServeAccessProtocol(&accessProtocolPlugin{})
}
