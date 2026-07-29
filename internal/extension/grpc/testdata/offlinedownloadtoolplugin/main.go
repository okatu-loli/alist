// Command offlinedownloadtoolplugin serves a synthetic
// Kind=OfflineDownloadTool extension as a go-plugin gRPC plugin (Phase E ring
// 1's mechanism-proving test double — v4 ships no real OfflineDownloadTool
// plugin yet). Unlike a one-return-value Kind (Notify/OnEvent, which prove
// wire fidelity by echoing their input back as the call's error message),
// AddURL/Status have real positive return values, so this fixture proves
// fidelity by encoding the received arguments directly into those return
// values instead: AddURL's gid embeds every ext.AddArgs field, Status's
// ext.ToolStatus fields are all derived from the requested gid, and Remove
// (which, like Notify, returns only an error) uses the same echo-via-error
// trick. Mirrors notificationplugin (testdata/notificationplugin) one Kind
// over.
package main

import (
	"context"
	"fmt"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type offlineDownloadToolPlugin struct{}

var _ ext.OfflineDownloadTool = (*offlineDownloadToolPlugin)(nil)

func (p *offlineDownloadToolPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testofflinedownloadtool",
		Name:    map[string]string{"en": "Test Offline Download Tool"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindOfflineDownloadTool},
	}
}

func (p *offlineDownloadToolPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *offlineDownloadToolPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *offlineDownloadToolPlugin) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: "echotool"}
}

// AddURL embeds every ext.AddArgs field into the returned gid, so a host test
// can decode it and assert the args crossed the wire intact.
func (p *offlineDownloadToolPlugin) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	return fmt.Sprintf("gid:%s:%s:%s", args.URL, args.TempDir, args.UID), nil
}

// Remove echoes gid back as the error message (mirrors Notify/OnEvent's
// echo-via-error trick, since Remove has no other return channel).
func (p *offlineDownloadToolPlugin) Remove(ctx context.Context, gid string) error {
	return fmt.Errorf("removed:%s", gid)
}

// Status derives every ext.ToolStatus field from gid (except for the
// "errgid" sentinel, which returns an error instead), so a host test can
// assert each field crossed the wire intact in both the success and error
// legs.
func (p *offlineDownloadToolPlugin) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	if gid == "errgid" {
		return ext.ToolStatus{}, fmt.Errorf("status error for %s", gid)
	}
	return ext.ToolStatus{
		TotalBytes: 12345,
		Progress:   42.5,
		NewGID:     "new-" + gid,
		Completed:  true,
		Status:     "status-for-" + gid,
		Err:        "err-for-" + gid,
	}, nil
}

func main() {
	grpcplugin.ServeOfflineDownloadTool(&offlineDownloadToolPlugin{})
}
