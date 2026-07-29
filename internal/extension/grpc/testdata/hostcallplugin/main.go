// Command hostcallplugin is the Task 6 production-path e2e fixture for the F0
// reverse-host bridge: a real StorageDriver subprocess whose Init(ctx, host) is
// invoked by the actual InitHost handshake (ext/grpcplugin's driverServer,
// dialled back through the go-plugin broker — not a synthetic probe protocol),
// and whose List then borrows the host capabilities it was handed:
// host.HTTP().Do against a target URL and host.LoadConfig. The outcome of each
// is encoded into the name of a returned Obj, so the host-side integration test
// can assert on them via the ordinary DriverService.List RPC.
//
// The HTTP target URL is read from HOSTCALLPLUGIN_TARGET_URL, inherited from
// the parent process — host.Launch/pluginproc leave exec.Cmd.Env nil, the same
// pattern testdata/authnplugin uses for AUTHNPLUGIN_MODE.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

func main() {
	grpcplugin.Serve(New())
}

// New builds an unstarted hostcallplugin driver; Init is what makes it usable.
func New() *Driver { return &Driver{} }

// Driver is a minimal ext.StorageDriver whose only interesting behavior is
// borrowing host capabilities in List after Init has stored the real host.
type Driver struct {
	host ext.Host
}

var _ ext.StorageDriver = (*Driver)(nil)

func (d *Driver) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.hostcallplugin",
		Name:    map[string]string{"en": "Host Call Test Driver"},
		Version: "0.1.0",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{
			Source:  "official",
			License: "MIT",
		},
	}
}

func (d *Driver) DriverConfig() ext.DriverConfig {
	return ext.DriverConfig{Name: "HostCallTest", OnlyProxy: false}
}

// Init is called exactly once by the reverse-bridge handshake: the host's
// GRPCClient stands up a HostService on a broker id and calls InitHost on the
// plugin, which dials that id, builds a host client, and calls Init with it —
// this is the production path under test, so a nil/absent Init here would mean
// the bridge never actually reached the plugin.
func (d *Driver) Init(ctx context.Context, host ext.Host) error {
	d.host = host
	return nil
}

func (d *Driver) Shutdown(ctx context.Context) error { return nil }

func (d *Driver) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}

func (d *Driver) DropInstance(ctx context.Context, storageID string) error { return nil }

// List borrows two host capabilities per call and folds the outcome of each
// into an Obj name:
//   - "http=<status>" when host.HTTP().Do succeeds against
//     HOSTCALLPLUGIN_TARGET_URL; "http=denied:<err>" when the host refuses it
//     (no network grant, or the target host is off the plugin's allowlist);
//     "http=no-target" if the env var isn't set.
//   - "config=<value>" with the raw bytes host.LoadConfig(ctx) returned;
//     "config=<nil>" if none was set; "config=err:<err>" on error.
func (d *Driver) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	if d.host == nil {
		return nil, ext.NewError(ext.ErrCodeNotSupport, "hostcallplugin: List called before Init (no host)")
	}

	httpName := "http=no-target"
	if target := os.Getenv("HOSTCALLPLUGIN_TARGET_URL"); target != "" {
		resp, err := d.host.HTTP().Do(ctx, &ext.HTTPRequest{Method: "GET", URL: target})
		if err != nil {
			httpName = fmt.Sprintf("http=denied:%s", err.Error())
		} else {
			httpName = fmt.Sprintf("http=%d", resp.Status)
		}
	}

	configName := "config=<nil>"
	if cfg, err := d.host.LoadConfig(ctx); err != nil {
		configName = fmt.Sprintf("config=err:%s", err.Error())
	} else if cfg != nil {
		configName = fmt.Sprintf("config=%s", string(cfg))
	}

	return []ext.Obj{
		{Name: httpName, ID: "http", Path: "/http"},
		{Name: configName, ID: "config", Path: "/config"},
	}, nil
}

func (d *Driver) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	return nil, ext.NewError(ext.ErrCodeNotImplement, "Link")
}
