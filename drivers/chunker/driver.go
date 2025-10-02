package chunker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"strings"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/utils"
)

type Chunker struct {
	model.Storage
	Addition

	nameFormat       *chunkNameFormat
	metaName         *metaNameFormat
	metaFormat       metadataFormat
	startIndex       int
	remoteStorage    driver.Driver
	remoteActualRoot string
}

func (d *Chunker) Config() driver.Config {
	return config
}

func (d *Chunker) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Chunker) Init(ctx context.Context) error {
	if d.ChunkSize <= 0 {
		d.ChunkSize = 2 << 30
	}
	if d.StartFrom == 0 {
		d.StartFrom = 1
	}
	nameFmt, err := compileNameFormat(utils.GetNoneEmpty(d.NameFormat, "{name}.rclone_chunk.{num:03d}"))
	if err != nil {
		return err
	}
	metaName := utils.GetNoneEmpty(d.MetaNameFormat, "{name}.rclone_chunk")
	metaFmt, err := compileMetaNameFormat(metaName)
	if err != nil {
		return err
	}
	metaFormat, err := parseMetaFormat(d.MetaFormat)
	if err != nil {
		return err
	}
	d.nameFormat = nameFmt
	d.metaName = metaFmt
	d.metaFormat = metaFormat
	d.startIndex = d.StartFrom
	d.RemotePath = sanitizePath(d.RemotePath)

	op.MustSaveDriverStorage(d)

	storage, err := fs.GetStorage(d.RemotePath, &fs.GetStoragesArgs{})
	if err != nil {
		return fmt.Errorf("failed to locate remote storage: %w", err)
	}
	d.remoteStorage = storage
	_, actualPath, err := op.GetStorageAndActualPath(d.RemotePath)
	if err != nil {
		return fmt.Errorf("failed to resolve remote actual path: %w", err)
	}
	d.remoteActualRoot = sanitizePath(actualPath)
	return nil
}

func (d *Chunker) Drop(ctx context.Context) error {
	d.remoteStorage = nil
	d.remoteActualRoot = ""
	d.nameFormat = nil
	d.metaName = nil
	return nil
}

func (d *Chunker) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	remotePath := d.remoteMountPathFor(dir.GetPath())
	remoteObjs, err := fs.List(ctx, remotePath, &fs.ListArgs{NoLog: true, Refresh: args.Refresh})
	if err != nil {
		return nil, err
	}
	objs, _, err := d.collectGroup(ctx, dir.GetPath(), remoteObjs)
	return objs, err
}

func (d *Chunker) Get(ctx context.Context, path string) (model.Obj, error) {
	if utils.PathEqual(path, "/") {
		return &model.Object{Name: "Root", Path: "/", IsFolder: true}, nil
	}
	dirPath, name := splitPath(path)
	remotePath := d.remoteMountPathFor(dirPath)
	remoteObjs, err := fs.List(ctx, remotePath, &fs.ListArgs{NoLog: true})
	if err != nil {
		return nil, err
	}
	objs, _, err := d.collectGroup(ctx, dirPath, remoteObjs)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if obj.GetName() == name {
			return obj, nil
		}
	}
	return nil, errs.ObjectNotFound
}

func (d *Chunker) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	if chunkObj, ok := unwrapChunkObject(file); ok {
		d.ensureMetaLoaded(ctx, chunkObj.group)
		rangeReader, err := d.chunkRangeReader(ctx, chunkObj.group, args)
		if err != nil {
			return nil, err
		}
		return &model.Link{RangeReadCloser: rangeReader}, nil
	}
	remoteActualPath := d.remoteActualPathFor(file.GetPath())
	link, _, err := op.Link(ctx, d.remoteStorage, remoteActualPath, args)
	if err != nil {
		return nil, err
	}
	return link, nil
}

func (d *Chunker) Remove(ctx context.Context, obj model.Obj) error {
	if chunkObj, ok := unwrapChunkObject(obj); ok {
		return d.removeGroup(ctx, chunkObj.group)
	}
	remoteActualPath := d.remoteActualPathFor(obj.GetPath())
	return op.Remove(ctx, d.remoteStorage, remoteActualPath)
}

func (d *Chunker) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	if chunkObj, ok := unwrapChunkObject(srcObj); ok {
		return d.renameGroup(ctx, chunkObj.group, newName)
	}
	remoteActualPath := d.remoteActualPathFor(srcObj.GetPath())
	return op.Rename(ctx, d.remoteStorage, remoteActualPath, newName)
}

func (d *Chunker) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	if chunkObj, ok := unwrapChunkObject(srcObj); ok {
		return d.moveGroup(ctx, chunkObj.group, dstDir)
	}
	remoteActualPath := d.remoteActualPathFor(srcObj.GetPath())
	dstActual := d.remoteActualPathFor(dstDir.GetPath())
	return op.Move(ctx, d.remoteStorage, remoteActualPath, dstActual)
}

func (d *Chunker) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	if chunkObj, ok := unwrapChunkObject(srcObj); ok {
		return d.copyGroup(ctx, chunkObj.group, dstDir)
	}
	remoteActualPath := d.remoteActualPathFor(srcObj.GetPath())
	dstActual := d.remoteActualPathFor(dstDir.GetPath())
	return op.Copy(ctx, d.remoteStorage, remoteActualPath, dstActual)
}

func (d *Chunker) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) error {
	defer file.Close()
	if d.remoteStorage == nil {
		return errors.New("chunker remote storage is not initialized")
	}
	dstActual := d.remoteActualPathFor(dstDir.GetPath())
	total := int64(0)
	expected := file.GetSize()
	index := d.startIndex
	var created []*chunkFile
	cleanup := func() {
		for _, ch := range created {
			_ = op.Remove(ctx, d.remoteStorage, ch.ActualPath)
		}
	}
	for {
		var limit int64 = d.ChunkSize
		if expected >= 0 {
			remaining := expected - total
			if remaining <= 0 {
				break
			}
			if remaining < limit {
				limit = remaining
			}
		}
		if limit <= 0 {
			break
		}
		chunkName := d.nameFormat.Format(file.GetName(), index)
		limited := &io.LimitedReader{R: file, N: limit}
		counter := &countingReader{Reader: limited}
		chunkObj := &model.Object{
			Name:     chunkName,
			Size:     limit,
			Modified: file.ModTime(),
			Ctime:    file.CreateTime(),
		}
		chunkStream := &stream.FileStream{
			Ctx:               ctx,
			Obj:               chunkObj,
			Reader:            counter,
			WebPutAsTask:      file.NeedStore(),
			ForceStreamUpload: file.IsForceStreamUpload(),
			Exist:             file.GetExist(),
		}
		var progress driver.UpdateProgress = up
		if progress != nil && expected > 0 {
			start := float64(total) / float64(expected) * 100
			end := float64(utils.Min(limit, expected-total)+total) / float64(expected) * 100
			progress = model.UpdateProgressWithRange(up, start, end)
		}
		if err := op.Put(ctx, d.remoteStorage, dstActual, chunkStream, progress); err != nil {
			cleanup()
			return err
		}
		readBytes := counter.Count()
		total += readBytes
		created = append(created, &chunkFile{
			Index:      index,
			Name:       chunkName,
			Size:       readBytes,
			Offset:     total - readBytes,
			ModTime:    file.ModTime(),
			CreateTime: file.CreateTime(),
			MountPath:  joinPath(d.remoteMountPathFor(dstDir.GetPath()), chunkName),
			ActualPath: joinPath(dstActual, chunkName),
		})
		index++
		if readBytes < limit {
			break
		}
	}
	if len(created) == 0 {
		// nothing uploaded
		return nil
	}
	// write metadata if required
	if d.metaFormat != metaFormatNone && (d.KeepMeta || len(created) > 1) {
		metaName := d.metaName.Format(file.GetName())
		data, err := d.marshalMetadata(total, len(created), file.GetHash())
		if err != nil {
			cleanup()
			return err
		}
		if len(data) > 0 {
			reader := io.NopCloser(bytes.NewReader(data))
			metaStream := &stream.FileStream{
				Ctx:               ctx,
				Obj:               &model.Object{Name: metaName, Size: int64(len(data)), Modified: file.ModTime()},
				Reader:            reader,
				ForceStreamUpload: true,
			}
			metaStream.Add(reader)
			if err := op.Put(ctx, d.remoteStorage, dstActual, metaStream, nil); err != nil {
				cleanup()
				return err
			}
		}
	}
	return nil
}

func splitPath(path string) (string, string) {
	path = sanitizePath(path)
	if utils.PathEqual(path, "/") {
		return "/", ""
	}
	dir, name := stdpath.Split(path)
	if dir == "" {
		dir = "/"
	}
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	dir = utils.FixAndCleanPath(dir)
	return dir, name
}

type countingReader struct {
	io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Count() int64 {
	return c.n
}

func (d *Chunker) removeGroup(ctx context.Context, group *compositeGroup) error {
	var errsJoined error
	for _, ch := range group.Files {
		if err := op.Remove(ctx, d.remoteStorage, ch.ActualPath); err != nil && !errs.IsObjectNotFound(err) {
			errsJoined = errors.Join(errsJoined, err)
		}
	}
	if group.Meta != nil {
		if err := op.Remove(ctx, d.remoteStorage, group.Meta.ActualPath); err != nil && !errs.IsObjectNotFound(err) {
			errsJoined = errors.Join(errsJoined, err)
		}
	}
	return errsJoined
}

func (d *Chunker) renameGroup(ctx context.Context, group *compositeGroup, newName string) error {
	baseDirActual := d.remoteActualPathFor(group.DirPath)
	baseDirMount := d.remoteMountPathFor(group.DirPath)
	for _, ch := range group.Files {
		newChunkName := d.nameFormat.Format(newName, ch.Index)
		if err := op.Rename(ctx, d.remoteStorage, ch.ActualPath, newChunkName); err != nil {
			return err
		}
		ch.Name = newChunkName
		ch.ActualPath = joinPath(baseDirActual, newChunkName)
		ch.MountPath = joinPath(baseDirMount, newChunkName)
	}
	if group.Meta != nil {
		newMetaName := d.metaName.Format(newName)
		if err := op.Rename(ctx, d.remoteStorage, group.Meta.ActualPath, newMetaName); err != nil {
			return err
		}
		group.Meta.ActualPath = joinPath(baseDirActual, newMetaName)
		group.Meta.MountPath = joinPath(baseDirMount, newMetaName)
		group.Meta.Name = newMetaName
	}
	return nil
}

func (d *Chunker) moveGroup(ctx context.Context, group *compositeGroup, dstDir model.Obj) error {
	dstActual := d.remoteActualPathFor(dstDir.GetPath())
	dstMount := d.remoteMountPathFor(dstDir.GetPath())
	for _, ch := range group.Files {
		if err := op.Move(ctx, d.remoteStorage, ch.ActualPath, dstActual); err != nil {
			return err
		}
		ch.ActualPath = joinPath(dstActual, ch.Name)
		ch.MountPath = joinPath(dstMount, ch.Name)
	}
	if group.Meta != nil {
		if err := op.Move(ctx, d.remoteStorage, group.Meta.ActualPath, dstActual); err != nil {
			return err
		}
		group.Meta.ActualPath = joinPath(dstActual, group.Meta.Name)
		group.Meta.MountPath = joinPath(dstMount, group.Meta.Name)
	}
	group.DirPath = dstDir.GetPath()
	return nil
}

func (d *Chunker) copyGroup(ctx context.Context, group *compositeGroup, dstDir model.Obj) error {
	dstActual := d.remoteActualPathFor(dstDir.GetPath())
	for _, ch := range group.Files {
		if err := op.Copy(ctx, d.remoteStorage, ch.ActualPath, dstActual); err != nil {
			return err
		}
	}
	if group.Meta != nil {
		if err := op.Copy(ctx, d.remoteStorage, group.Meta.ActualPath, dstActual); err != nil {
			return err
		}
	}
	return nil
}

var _ interface {
	driver.Driver
	driver.Getter
	driver.Put
	driver.Move
	driver.Copy
	driver.Remove
	driver.Rename
} = (*Chunker)(nil)
