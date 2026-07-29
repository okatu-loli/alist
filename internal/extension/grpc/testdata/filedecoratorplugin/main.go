// Command filedecoratorplugin serves a synthetic Kind=FileDecorator extension
// as a go-plugin gRPC plugin (the extension-field-group mechanism-proving test
// double — v4 ships no real FileDecorator plugin yet). It declares one
// FileColumn ("_labels") and Decorate returns a value per ref derived from the
// group and the ref's RefKey, so a host test can prove the full Decorate round
// trip (group + refs in, values map out) against a real subprocess. Mirrors
// eventhookplugin (testdata/eventhookplugin) one Kind over.
package main

import (
	"context"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type fileDecoratorPlugin struct{}

var _ ext.FileDecorator = (*fileDecoratorPlugin)(nil)

func (p *fileDecoratorPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testfiledecorator",
		Name:    map[string]string{"en": "Test File Decorator"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindFileDecorator},
		Contributions: ext.Contributions{
			FileColumns: []ext.FileColumn{
				{ID: "labels", Header: map[string]string{"en": "Labels"}, FieldGroup: "_labels"},
			},
		},
	}
}

func (p *fileDecoratorPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (p *fileDecoratorPlugin) Shutdown(ctx context.Context) error         { return nil }

func (p *fileDecoratorPlugin) Groups() []string { return []string{"_labels"} }

func (p *fileDecoratorPlugin) Decorate(ctx context.Context, group string, refs []ext.ObjRef) (map[string]any, error) {
	out := map[string]any{}
	for _, ref := range refs {
		out[ext.RefKey(ref)] = "decorated:" + group
	}
	return out, nil
}

func main() {
	grpcplugin.ServeFileDecorator(&fileDecoratorPlugin{})
}
