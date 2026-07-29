// Package urldriver is a synthetic ext-native storage driver that returns
// URL-form Links. It exercises the ByteSource==url branch of the 9.2 protocol
// parity (the local driver covers the stream branch). It is a real
// ext.StorageDriver — not a bridge over a legacy driver — so it also proves the
// ext-native path end-to-end.
//
// It is intentionally not a ported real cloud driver: porting one is 9.4 scope.
package urldriver

import (
	"context"
	"encoding/json"
	"net/http"

	ext "github.com/AlistGo/alist-ext"
)

// New builds a synthetic URL driver with a fixed in-memory tree.
func New() *Driver { return &Driver{} }

// Driver serves a fixed tree: the root contains one file, "hello.txt", whose
// Link is a URL with a custom header.
type Driver struct{}

const (
	// FileAnchor is the anchor of the single file the driver serves.
	FileAnchor = "hello.txt"
	// FileURL is the URL its Link resolves to.
	FileURL = "https://cdn.example.com/hello.txt"
	// HeaderKey / HeaderVal is the custom header carried on the Link.
	HeaderKey = "X-Ext-Token"
	HeaderVal = "abc123"
)

var (
	_ ext.StorageDriver = (*Driver)(nil)
	_ ext.Getter        = (*Driver)(nil)
	_ ext.RapidPutter   = (*Driver)(nil)
)

func (d *Driver) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:           "com.example.urldriver",
		Name:         map[string]string{"en": "URL Test Driver"},
		Version:      "0.1.0",
		CoreAPI:      "^4.0.0",
		Kinds:        []ext.Kind{ext.KindStorageDriver},
		Capabilities: []ext.Capability{ext.CapGetter, ext.CapPutRapid},
		Provenance:   ext.Provenance{Source: "official", License: "MIT"},
	}
}

func (d *Driver) DriverConfig() ext.DriverConfig {
	return ext.DriverConfig{Name: "URLTest", OnlyProxy: false}
}

func (d *Driver) Init(ctx context.Context, host ext.Host) error { return nil }
func (d *Driver) Shutdown(ctx context.Context) error            { return nil }

func (d *Driver) NewInstance(ctx context.Context, storageID string, config json.RawMessage) error {
	return nil
}
func (d *Driver) DropInstance(ctx context.Context, storageID string) error { return nil }

func (d *Driver) List(ctx context.Context, dir ext.ObjRef, args ext.ListArgs) ([]ext.Obj, error) {
	return []ext.Obj{{
		Name:     FileAnchor,
		Size:     11,
		IsDir:    false,
		ID:       FileAnchor,
		Path:     "/" + FileAnchor,
		Modified: 1_700_000_000_000,
	}}, nil
}

func (d *Driver) Link(ctx context.Context, file ext.ObjRef, args ext.LinkArgs) (*ext.Link, error) {
	if file.Anchor != FileAnchor {
		return nil, ext.NewError(ext.ErrCodeObjectNotFound, file.Anchor)
	}
	return &ext.Link{
		ByteSource:   ext.ByteSourceURL,
		URL:          FileURL,
		Header:       http.Header{HeaderKey: []string{HeaderVal}},
		RangeSupport: true,
		Expiration:   3600,
	}, nil
}

func (d *Driver) Get(ctx context.Context, storageID, path string) (ext.Obj, error) {
	if path != FileAnchor && path != "/"+FileAnchor {
		return ext.Obj{}, ext.NewError(ext.ErrCodeObjectNotFound, path)
	}
	return ext.Obj{Name: FileAnchor, Size: 11, ID: FileAnchor, Path: "/" + FileAnchor}, nil
}

// RapidMagicMD5 is the synthetic md5 TryRapidUpload treats as a hit, so
// bridge tests can drive both outcomes (hit/miss) deterministically without a
// real content-addressed backing store.
const RapidMagicMD5 = "0123456789abcdef0123456789abcdef"

// RapidMagicMD5EmptyObj is a second synthetic hit md5: the driver reports a
// hit but hands back a wholly empty ext.Obj (no Name, no ID), the wire shape
// of "we registered the file by hash and have nothing else to return". It
// exercises pluginDriver.TryRapidUpload's ghost-object guard (M1): the host
// must report the hit (true) but hand op.PutRapid a nil model.Obj, not a
// non-nil all-zero-value one that would get wrongly cached as a real entry.
const RapidMagicMD5EmptyObj = "fedcba9876543210fedcba9876543210"

// TryRapidUpload: synthetic rapid — hits iff the caller presents
// RapidMagicMD5 (or RapidMagicMD5EmptyObj, see its doc), so bridge tests can
// drive all three outcomes deterministically.
func (d *Driver) TryRapidUpload(ctx context.Context, dstDir ext.ObjRef, name string, size int64, hashes map[string]string) (ext.Obj, bool, error) {
	switch hashes["md5"] {
	case RapidMagicMD5:
		return ext.Obj{Name: name, Size: size, ID: "rapid-" + name, Hashes: hashes}, true, nil
	case RapidMagicMD5EmptyObj:
		return ext.Obj{}, true, nil
	}
	return ext.Obj{}, false, nil
}
