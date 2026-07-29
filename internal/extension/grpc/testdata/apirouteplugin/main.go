// Command apirouteplugin serves a synthetic Kind=APIRoute extension as a
// go-plugin gRPC plugin (the webhook/share-API mechanism-proving test double —
// v4 ships no real webhook/share plugin yet). It declares a fixed route set and
// echoes the request method/URL/body back in its Handle response, so a host
// test can prove the full DispatchAPIRoute round trip against a real
// subprocess. Mirrors bgplugin (testdata/bgplugin) one Kind over.
package main

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type apiRoutePlugin struct{}

var _ ext.APIRoute = (*apiRoutePlugin)(nil)

func (p *apiRoutePlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testapiroute",
		Name:    map[string]string{"en": "Test API Route"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindAPIRoute},
	}
}

func (p *apiRoutePlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *apiRoutePlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *apiRoutePlugin) Routes() []ext.RouteDecl {
	return []ext.RouteDecl{
		{Prefix: "/webhook"},
		{Prefix: "/s", Public: true},
	}
}

func (p *apiRoutePlugin) Handle(ctx context.Context, req *ext.HTTPRequest) (*ext.HTTPResponse, error) {
	return &ext.HTTPResponse{
		Status: 200,
		Body:   []byte("handled:" + req.Method + ":" + req.URL),
	}, nil
}

func main() {
	grpcplugin.ServeAPIRoute(&apiRoutePlugin{})
}
