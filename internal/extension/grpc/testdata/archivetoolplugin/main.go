// Command archivetoolplugin serves a synthetic Kind=ArchiveTool extension as a
// go-plugin gRPC plugin (Phase E wave 2's mechanism-proving test double — v4
// ships no real third-party ArchiveTool plugin yet). It ignores the
// ArchiveSource it is handed (a real ArchiveTool would parse src.Path's bytes;
// this fixture doesn't need real archive parsing to prove the wire round
// trip) and instead serves a small FIXED virtual layout:
//
//	hello.txt        "hello" (5 bytes, root)
//	sub/world.txt     "world!" (6 bytes, nested)
//
// so Meta/List/Extract behave enough to prove Extract's streaming shape
// (first chunk carries size, then data) end to end. Mirrors
// searchindexerplugin (testdata/searchindexerplugin) one Kind over.
package main

import (
	"bytes"
	"context"
	"io"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

type archiveToolPlugin struct{}

var _ ext.ArchiveTool = archiveToolPlugin{}

func (archiveToolPlugin) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.testarchivetool",
		Name:    map[string]string{"en": "Test Archive Tool"},
		Version: "0.0.1",
		CoreAPI: "^4.0.0",
		Kinds:   []ext.Kind{ext.KindArchiveTool},
	}
}

func (archiveToolPlugin) Init(ctx context.Context, h ext.Host) error { return nil }
func (archiveToolPlugin) Shutdown(ctx context.Context) error         { return nil }

func (archiveToolPlugin) Exts() []string { return []string{"fx"} }

func (archiveToolPlugin) Meta(ctx context.Context, src ext.ArchiveSource, password string) (ext.ArchiveToolMeta, error) {
	return ext.ArchiveToolMeta{Comment: "fixture", EntryCount: 2, TotalUncompressedSize: 11}, nil
}

func (archiveToolPlugin) List(ctx context.Context, src ext.ArchiveSource, innerPath, password string) ([]ext.ArchiveEntry, error) {
	switch innerPath {
	case "":
		return []ext.ArchiveEntry{
			{Name: "hello.txt", Size: 5},
			{Name: "sub", IsDir: true},
		}, nil
	case "sub":
		return []ext.ArchiveEntry{
			{Name: "world.txt", Size: 6},
		}, nil
	default:
		return nil, ext.NewError(ext.ErrCodeObjectNotFound, innerPath)
	}
}

func (archiveToolPlugin) Extract(ctx context.Context, src ext.ArchiveSource, innerPath, password string) (io.ReadCloser, int64, error) {
	switch innerPath {
	case "hello.txt":
		return io.NopCloser(bytes.NewReader([]byte("hello"))), 5, nil
	case "sub/world.txt":
		return io.NopCloser(bytes.NewReader([]byte("world!"))), 6, nil
	case "sub":
		return nil, 0, ext.NewError(ext.ErrCodeArchiveIsDir, innerPath)
	default:
		return nil, 0, ext.NewError(ext.ErrCodeObjectNotFound, innerPath)
	}
}

func main() {
	grpcplugin.ServeArchiveTool(archiveToolPlugin{})
}
