package chunker

import (
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/op"
)

type Addition struct {
	RemotePath     string `json:"remote_path" required:"true" help:"Mount path of wrapped storage"`
	ChunkSize      int64  `json:"chunk_size" type:"size" default:"2147483648" help:"Size of a single chunk"`
	NameFormat     string `json:"name_format" default:"{name}.rclone_chunk.{num:03d}" help:"Pattern used to name chunk objects"`
	MetaNameFormat string `json:"meta_name_format" default:"{name}.rclone_chunk" help:"Pattern used to name metadata objects"`
	StartFrom      int    `json:"start_from" default:"1" help:"Starting index for chunk numbering"`
	MetaFormat     string `json:"meta_format" type:"select" options:"simplejson,none" default:"simplejson" help:"Metadata serialization format"`
	KeepMeta       bool   `json:"keep_meta" default:"true" help:"Keep metadata objects for single chunk files"`
}

var config = driver.Config{
	Name:              "Chunker",
	OnlyLocal:         false,
	OnlyProxy:         false,
	LocalSort:         true,
	NoCache:           true,
	NoUpload:          false,
	NeedMs:            false,
	DefaultRoot:       "/",
	CheckStatus:       false,
	Alert:             "",
	NoOverwriteUpload: false,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Chunker{}
	})
}
