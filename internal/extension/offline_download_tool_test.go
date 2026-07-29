package extension

import (
	"context"
	"reflect"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// stubOfflineDownloadTool is a minimal ext.OfflineDownloadTool for registry
// tests. Only Config().Name matters here; the daemon methods are no-ops.
type stubOfflineDownloadTool struct{ name string }

func (s stubOfflineDownloadTool) Manifest() ext.Manifest                     { return ext.Manifest{ID: s.name} }
func (s stubOfflineDownloadTool) Init(ctx context.Context, h ext.Host) error { return nil }
func (s stubOfflineDownloadTool) Shutdown(ctx context.Context) error         { return nil }
func (s stubOfflineDownloadTool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: s.name}
}
func (s stubOfflineDownloadTool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	return "", nil
}
func (s stubOfflineDownloadTool) Remove(ctx context.Context, gid string) error { return nil }
func (s stubOfflineDownloadTool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	return ext.ToolStatus{}, nil
}

func TestOfflineDownloadToolRegistry(t *testing.T) {
	// Clean any state from other tests in this package sharing the registry.
	for _, id := range OfflineDownloadToolInstanceIDs() {
		RegisterTrustedOfflineDownloadToolInstance(id, nil)
	}

	fake := stubOfflineDownloadTool{name: "faketool"}
	if _, ok := GetOfflineDownloadTool("faketool"); ok {
		t.Fatal("GetOfflineDownloadTool returned an instance before registration")
	}

	RegisterTrustedOfflineDownloadToolInstance("faketool", fake)
	got, ok := GetOfflineDownloadTool("faketool")
	if !ok {
		t.Fatal("GetOfflineDownloadTool(faketool) not found after registration")
	}
	if got.Config().Name != "faketool" {
		t.Fatalf("resolved instance Config().Name = %q, want faketool", got.Config().Name)
	}

	// nil deregisters.
	RegisterTrustedOfflineDownloadToolInstance("faketool", nil)
	if _, ok := GetOfflineDownloadTool("faketool"); ok {
		t.Fatal("GetOfflineDownloadTool(faketool) still resolves after nil deregistration")
	}
}

func TestOfflineDownloadToolInstanceIDsSorted(t *testing.T) {
	for _, id := range OfflineDownloadToolInstanceIDs() {
		RegisterTrustedOfflineDownloadToolInstance(id, nil)
	}
	RegisterTrustedOfflineDownloadToolInstance("Transmission", stubOfflineDownloadTool{name: "Transmission"})
	RegisterTrustedOfflineDownloadToolInstance("aria2", stubOfflineDownloadTool{name: "aria2"})
	RegisterTrustedOfflineDownloadToolInstance("qBittorrent", stubOfflineDownloadTool{name: "qBittorrent"})
	t.Cleanup(func() {
		for _, id := range []string{"Transmission", "aria2", "qBittorrent"} {
			RegisterTrustedOfflineDownloadToolInstance(id, nil)
		}
	})

	got := OfflineDownloadToolInstanceIDs()
	want := []string{"Transmission", "aria2", "qBittorrent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OfflineDownloadToolInstanceIDs() = %v, want sorted %v", got, want)
	}
}
