package offline_download

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/extension"
)

// TestToolByNameFallsBackToGRPCTool pins the ring-8 miss fallback: a
// Kind=OfflineDownloadTool gRPC plugin entry (no RegisterTool mapping, since
// only built-in tools get one) is still resolved by ToolByName as long as its
// self-reported Config().Name matches, through
// extension.FindOfflineDownloadToolByName. Disabling the plugin entry gates it
// back to nil, the same enabled-gate the built-in path already has.
func TestToolByNameFallsBackToGRPCTool(t *testing.T) {
	resetBase(t)

	const pluginID = "com.example.grpctool"
	const toolName = "grpctool"

	if err := extension.DefaultRegistry.RegisterPlugin(ext.Manifest{
		ID:    pluginID,
		Name:  map[string]string{"en": "gRPC Tool"},
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}, "unused-binpath"); err != nil {
		t.Fatalf("RegisterPlugin: %v", err)
	}
	t.Cleanup(func() { extension.DefaultRegistry.Deregister(pluginID) })

	stub := &baseFakeTool{name: toolName}
	extension.SetOfflineDownloadToolFactory(func(e *extension.Entry) (ext.OfflineDownloadTool, func(), error) {
		return stub, func() {}, nil
	})
	t.Cleanup(func() { extension.SetOfflineDownloadToolFactory(nil) })

	if ToolByName(toolName) != ext.OfflineDownloadTool(stub) {
		t.Fatal("a gRPC-registered tool must resolve by its Config().Name via the miss fallback")
	}

	if err := extension.DefaultRegistry.Disable(pluginID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if ToolByName(toolName) != nil {
		t.Fatal("a disabled gRPC tool plugin must gate ToolByName to nil")
	}
}
