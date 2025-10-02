package chunker

import (
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
)

type metadataFormat int

const (
	metaFormatNone metadataFormat = iota
	metaFormatSimpleJSON
)

type chunkMeta struct {
	Version  int
	Size     int64
	ChunkNum int
	MD5      string
	SHA1     string
	Txn      string
}

type chunkFile struct {
	Index      int
	Name       string
	Size       int64
	Offset     int64
	ModTime    time.Time
	CreateTime time.Time
	Hash       utils.HashInfo
	MountPath  string
	ActualPath string
}

type metaFile struct {
	Name       string
	MountPath  string
	ActualPath string
	Obj        model.Obj
	Meta       *chunkMeta
}

type compositeGroup struct {
	DirPath   string
	BaseName  string
	Files     []*chunkFile
	Meta      *metaFile
	TotalSize int64
	HashInfo  utils.HashInfo
}

type chunkObject struct {
	model.Object
	group *compositeGroup
}

func (o *chunkObject) Group() *compositeGroup {
	return o.group
}

func wrapChunkObject(obj *model.Object, group *compositeGroup) *chunkObject {
	return &chunkObject{Object: *obj, group: group}
}

func unwrapChunkObject(obj model.Obj) (*chunkObject, bool) {
	res, ok := obj.(*chunkObject)
	if ok {
		return res, true
	}
	if wrapper, ok := obj.(*model.ObjWrapName); ok {
		return unwrapChunkObject(wrapper.Unwrap())
	}
	return nil, false
}
