package ext

import (
	"context"
	"testing"
)

type stubAPIRoute struct{}

func (stubAPIRoute) Manifest() Manifest               { return Manifest{ID: "stub.api"} }
func (stubAPIRoute) Init(context.Context, Host) error { return nil }
func (stubAPIRoute) Shutdown(context.Context) error   { return nil }
func (stubAPIRoute) Routes() []RouteDecl {
	return []RouteDecl{{Prefix: "/webhook"}, {Prefix: "/s", Public: true}}
}
func (stubAPIRoute) Handle(_ context.Context, req *HTTPRequest) (*HTTPResponse, error) {
	return &HTTPResponse{Status: 200, Body: []byte("path=" + req.URL)}, nil
}

func TestAPIRouteInterfaceShape(t *testing.T) {
	var a APIRoute = stubAPIRoute{}
	if got := a.Routes(); len(got) != 2 || !got[1].Public {
		t.Fatalf("Routes: %+v", got)
	}
	resp, err := a.Handle(context.Background(), &HTTPRequest{Method: "POST", URL: "/webhook/x"})
	if err != nil || resp.Status != 200 || string(resp.Body) != "path=/webhook/x" {
		t.Fatalf("Handle: %+v err=%v", resp, err)
	}
}
