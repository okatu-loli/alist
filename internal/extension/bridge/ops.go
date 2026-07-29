package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"time"

	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// The bridge presents every optional op it knows how to translate. Each method
// delegates to the legacy driver's matching interface and returns ext
// NOT_IMPLEMENT when the underlying driver lacks the capability, so the frozen
// Manifest.Capabilities (derived from the legacy interfaces) stays the source of
// truth even though *StorageBridge always satisfies the ext optional interfaces
// at the Go type level.

var (
	_ ext.Getter      = (*StorageBridge)(nil)
	_ ext.Mkdirer     = (*StorageBridge)(nil)
	_ ext.Mover       = (*StorageBridge)(nil)
	_ ext.Renamer     = (*StorageBridge)(nil)
	_ ext.Copier      = (*StorageBridge)(nil)
	_ ext.Remover     = (*StorageBridge)(nil)
	_ ext.Putter      = (*StorageBridge)(nil)
	_ ext.RapidPutter = (*StorageBridge)(nil)
)

func (b *StorageBridge) Get(ctx context.Context, storageID, path string) (ext.Obj, error) {
	d, err := b.instance(storageID)
	if err != nil {
		return ext.Obj{}, err
	}
	g, ok := d.(driver.Getter)
	if !ok {
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Get")
	}
	o, err := g.Get(ctx, path)
	if err != nil {
		return ext.Obj{}, err
	}
	return objToExt(o), nil
}

func (b *StorageBridge) Mkdir(ctx context.Context, parent ext.ObjRef, name string) (ext.Obj, error) {
	d, err := b.instance(parent.StorageID)
	if err != nil {
		return ext.Obj{}, err
	}
	switch s := d.(type) {
	case driver.MkdirResult:
		o, err := s.MakeDir(ctx, refToObj(parent), name)
		if err != nil {
			return ext.Obj{}, err
		}
		return objToExt(o), nil
	case driver.Mkdir:
		if err := s.MakeDir(ctx, refToObj(parent), name); err != nil {
			return ext.Obj{}, err
		}
		return ext.Obj{Name: name, IsDir: true}, nil
	default:
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Mkdir")
	}
}

func (b *StorageBridge) Move(ctx context.Context, src, dstDir ext.ObjRef) (ext.Obj, error) {
	d, err := b.instance(src.StorageID)
	if err != nil {
		return ext.Obj{}, err
	}
	switch s := d.(type) {
	case driver.MoveResult:
		o, err := s.Move(ctx, refToObj(src), refToObj(dstDir))
		if err != nil {
			return ext.Obj{}, err
		}
		return objToExt(o), nil
	case driver.Move:
		if err := s.Move(ctx, refToObj(src), refToObj(dstDir)); err != nil {
			return ext.Obj{}, err
		}
		return ext.Obj{}, nil
	default:
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Move")
	}
}

func (b *StorageBridge) Rename(ctx context.Context, src ext.ObjRef, newName string) (ext.Obj, error) {
	d, err := b.instance(src.StorageID)
	if err != nil {
		return ext.Obj{}, err
	}
	switch s := d.(type) {
	case driver.RenameResult:
		o, err := s.Rename(ctx, refToObj(src), newName)
		if err != nil {
			return ext.Obj{}, err
		}
		return objToExt(o), nil
	case driver.Rename:
		if err := s.Rename(ctx, refToObj(src), newName); err != nil {
			return ext.Obj{}, err
		}
		return ext.Obj{Name: newName}, nil
	default:
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Rename")
	}
}

func (b *StorageBridge) Copy(ctx context.Context, src, dstDir ext.ObjRef) (ext.Obj, error) {
	d, err := b.instance(src.StorageID)
	if err != nil {
		return ext.Obj{}, err
	}
	switch s := d.(type) {
	case driver.CopyResult:
		o, err := s.Copy(ctx, refToObj(src), refToObj(dstDir))
		if err != nil {
			return ext.Obj{}, err
		}
		return objToExt(o), nil
	case driver.Copy:
		if err := s.Copy(ctx, refToObj(src), refToObj(dstDir)); err != nil {
			return ext.Obj{}, err
		}
		return ext.Obj{}, nil
	default:
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Copy")
	}
}

// TryRapidUpload implements ext.RapidPutter over a legacy driver.PutRapid
// (SP-H). Miss is (Obj{}, false, nil); a legacy errs.NotSupport /
// errs.NotImplement is re-coded as the typed ext error so the host maps it
// back losslessly.
func (b *StorageBridge) TryRapidUpload(ctx context.Context, dstDir ext.ObjRef, name string, size int64, hashes map[string]string) (ext.Obj, bool, error) {
	d, err := b.instance(dstDir.StorageID)
	if err != nil {
		return ext.Obj{}, false, err
	}
	rp, ok := d.(driver.PutRapid)
	if !ok {
		return ext.Obj{}, false, ext.NewError(ext.ErrCodeNotImplement, "TryRapidUpload")
	}
	o, hit, err := rp.TryRapidUpload(ctx, refToObj(dstDir), name, size, hashesFromMap(hashes))
	if err != nil {
		switch {
		case errors.Is(err, errs.NotSupport):
			return ext.Obj{}, false, ext.NewError(ext.ErrCodeNotSupport, err.Error())
		case errors.Is(err, errs.NotImplement):
			return ext.Obj{}, false, ext.NewError(ext.ErrCodeNotImplement, err.Error())
		}
		return ext.Obj{}, false, err
	}
	if !hit {
		return ext.Obj{}, false, nil
	}
	if o == nil {
		// A legacy driver.PutRapid may report a hit without an object (e.g. it
		// only registered the file by hash and has nothing else to hand back).
		// That is a real hit, not a miss — collapsing it to (Obj{}, false, nil)
		// would tell the plugin-side caller (op.PutRapid, via pluginDriver) no
		// match was found, and it would stream the whole file again even
		// though the rapid upload already succeeded. See
		// TestStorageBridgeTryRapidUploadHitWithNilObj.
		return ext.Obj{}, true, nil
	}
	return objToExt(o), true, nil
}

func (b *StorageBridge) Remove(ctx context.Context, obj ext.ObjRef) error {
	d, err := b.instance(obj.StorageID)
	if err != nil {
		return err
	}
	r, ok := d.(driver.Remove)
	if !ok {
		return ext.NewError(ext.ErrCodeNotImplement, "Remove")
	}
	return r.Remove(ctx, refToObj(obj))
}

func (b *StorageBridge) Put(ctx context.Context, dstDir ext.ObjRef, up ext.UploadStream) (ext.Obj, error) {
	d, err := b.instance(dstDir.StorageID)
	if err != nil {
		return ext.Obj{}, err
	}
	info := up.Info()
	fs := &stream.FileStream{
		Ctx: ctx,
		Obj: &model.Object{
			Name:     info.Name,
			Size:     info.Size,
			Modified: time.Now(),
		},
		Reader:   &uploadStreamReader{up: up, size: info.Size},
		Mimetype: info.Mimetype,
	}
	defer fs.Close()
	noop := func(float64) {}

	switch s := d.(type) {
	case driver.PutResult:
		o, err := s.Put(ctx, refToObj(dstDir), fs, noop)
		if err != nil {
			return ext.Obj{}, err
		}
		if o != nil {
			return objToExt(o), nil
		}
		return ext.Obj{Name: info.Name, Size: info.Size}, nil
	case driver.Put:
		if err := s.Put(ctx, refToObj(dstDir), fs, noop); err != nil {
			return ext.Obj{}, err
		}
		return ext.Obj{Name: info.Name, Size: info.Size}, nil
	default:
		return ext.Obj{}, ext.NewError(ext.ErrCodeNotImplement, "Put")
	}
}

// uploadStreamReader adapts the pull-model ext.UploadStream (ReadAt) into the
// sequential io.Reader legacy drivers expect (9.0 §2 bridge).
type uploadStreamReader struct {
	up   ext.UploadStream
	off  int64
	size int64
}

func (r *uploadStreamReader) Read(p []byte) (int, error) {
	if r.size >= 0 && r.off >= r.size {
		return 0, io.EOF
	}
	n, err := r.up.ReadAt(p, r.off)
	r.off += int64(n)
	return n, err
}

// --- helpers ---

// refToObj builds the minimal legacy model.Obj a path-anchored driver needs from
// an ObjRef. The Name is the anchor's basename so drivers that derive a
// destination filename from GetName() (local Move/Copy) work. 9.1 note: the
// Anchor carries the driver-internal path for path-anchored drivers (local).
// ID-anchored drivers land with the 9.2 wire codec; this bridge covers the
// in-process path-anchored case. Seam.
func refToObj(ref ext.ObjRef) model.Obj {
	return &model.Object{Path: ref.Anchor, ID: ref.Anchor, Name: path.Base(ref.Anchor)}
}

func objToExt(o model.Obj) ext.Obj {
	return ext.Obj{
		Name:     o.GetName(),
		Size:     o.GetSize(),
		Modified: unixMilli(o.ModTime()),
		Created:  unixMilli(o.CreateTime()),
		IsDir:    o.IsDir(),
		ID:       o.GetID(),
		Path:     o.GetPath(),
		Hashes:   hashesToMap(o.GetHash()),
	}
}

// hashesToMap flattens utils.HashInfo into the wire map (keys = registered
// hash names "md5"/"sha1"/...). nil for empty so proto/json stay clean.
func hashesToMap(hi utils.HashInfo) map[string]string {
	var m map[string]string
	for ht, v := range hi.All() {
		if m == nil {
			m = make(map[string]string)
		}
		m[ht.Name] = v
	}
	return m
}

// hashesFromMap rebuilds utils.HashInfo from the wire map by looking each
// name up directly in the hash registry (utils.GetHashByName) — no
// json.Marshal/Unmarshal round-trip through utils.FromString needed, since
// the wire map is already the map[string]string shape FromString itself
// decodes into. Unknown hash names are dropped (same behavior as v3
// persistence via FromString); values whose length doesn't match the
// registered algorithm's digest width are also dropped, since a bare
// registry lookup only tells us the name is known, not that the value is a
// well-formed digest. Without the width check, a plugin (buggy or
// malicious) could hand back a chunk/partial hash or a truncated digest,
// and the host would content-address on it as if it were an exact
// whole-file hash (ext.Obj.Hashes's contract) — silently serving/matching
// the wrong content. See TestHashesFromMapRejectsWrongWidth.
func hashesFromMap(m map[string]string) utils.HashInfo {
	if len(m) == 0 {
		return utils.HashInfo{}
	}
	filtered := make(map[*utils.HashType]string, len(m))
	for name, v := range m {
		ht := utils.GetHashByName(name)
		if ht == nil {
			continue
		}
		if len(v) == ht.Width {
			filtered[ht] = v
		}
	}
	return utils.NewHashInfoByMap(filtered)
}

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func httpRange(start, length int64) http_range.Range {
	return http_range.Range{Start: start, Length: length}
}

func utilsUnmarshalAddition(d driver.Driver, config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	return utils.Json.Unmarshal(config, d.GetAddition())
}
