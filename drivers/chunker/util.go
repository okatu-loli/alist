package chunker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdpath "path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/http_range"
	"github.com/alist-org/alist/v3/pkg/utils"
)

var (
	nameTokenRegexp = regexp.MustCompile(`\{name\}`)
	numTokenRegexp  = regexp.MustCompile(`\{num(?::0(\d+)d)?\}`)
)

type chunkNameFormat struct {
	raw           string
	numToken      string
	width         int
	compiledRegex *regexp.Regexp
}

type metaNameFormat struct {
	raw           string
	compiledRegex *regexp.Regexp
}

func compileNameFormat(format string) (*chunkNameFormat, error) {
	if format == "" {
		return nil, errors.New("name format is empty")
	}
	if !nameTokenRegexp.MatchString(format) {
		return nil, errors.New("name format must contain {name}")
	}
	numToken := numTokenRegexp.FindString(format)
	if numToken == "" {
		return nil, errors.New("name format must contain {num}")
	}
	matches := numTokenRegexp.FindStringSubmatch(format)
	width := 0
	if len(matches) > 1 && matches[1] != "" {
		n, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("invalid num width: %w", err)
		}
		width = n
	}
	if strings.Count(format, "{name}") != 1 {
		return nil, errors.New("name format must contain one {name}")
	}
	if strings.Count(format, numToken) != 1 {
		return nil, errors.New("name format must contain one {num}")
	}
	var regexBuilder strings.Builder
	last := 0
	for last < len(format) {
		idxName := strings.Index(format[last:], "{name}")
		idxNum := strings.Index(format[last:], numToken)
		next := len(format)
		placeholder := ""
		if idxName >= 0 && (idxName < idxNum || idxNum < 0) {
			next = last + idxName
			placeholder = "{name}"
		} else if idxNum >= 0 {
			next = last + idxNum
			placeholder = numToken
		}
		if next > last {
			regexBuilder.WriteString(regexp.QuoteMeta(format[last:next]))
		}
		if placeholder == "{name}" {
			regexBuilder.WriteString(`(?P<name>.+?)`)
			last = next + len("{name}")
			continue
		}
		if placeholder == numToken {
			if width > 0 {
				regexBuilder.WriteString(fmt.Sprintf(`(?P<num>\d{%d,})`, width))
			} else {
				regexBuilder.WriteString(`(?P<num>\d+)`)
			}
			last = next + len(numToken)
			continue
		}
		if next == len(format) {
			break
		}
	}
	if last < len(format) {
		regexBuilder.WriteString(regexp.QuoteMeta(format[last:]))
	}
	compiled, err := regexp.Compile("^" + regexBuilder.String() + "$")
	if err != nil {
		return nil, err
	}
	return &chunkNameFormat{raw: format, numToken: numToken, width: width, compiledRegex: compiled}, nil
}

func (f *chunkNameFormat) Format(name string, index int) string {
	num := strconv.Itoa(index)
	if f.width > 0 {
		num = fmt.Sprintf("%0*d", f.width, index)
	}
	out := strings.ReplaceAll(f.raw, "{name}", name)
	out = strings.Replace(out, f.numToken, num, 1)
	return out
}

func (f *chunkNameFormat) Match(filename string) (base string, idx int, ok bool) {
	if f == nil || f.compiledRegex == nil {
		return "", 0, false
	}
	matches := f.compiledRegex.FindStringSubmatch(filename)
	if matches == nil {
		return "", 0, false
	}
	base = matches[f.compiledRegex.SubexpIndex("name")]
	numStr := matches[f.compiledRegex.SubexpIndex("num")]
	if numStr == "" {
		return "", 0, false
	}
	value, err := strconv.Atoi(numStr)
	if err != nil {
		return "", 0, false
	}
	return base, value, true
}

func compileMetaNameFormat(format string) (*metaNameFormat, error) {
	if format == "" {
		return nil, errors.New("meta name format is empty")
	}
	if strings.Count(format, "{name}") != 1 {
		return nil, errors.New("meta name format must contain {name}")
	}
	parts := strings.Split(format, "{name}")
	if len(parts) != 2 {
		return nil, errors.New("meta name format must contain exactly one {name}")
	}
	pattern := regexp.QuoteMeta(parts[0]) + `(?P<name>.+?)` + regexp.QuoteMeta(parts[1])
	compiled, err := regexp.Compile("^" + pattern + "$")
	if err != nil {
		return nil, err
	}
	return &metaNameFormat{raw: format, compiledRegex: compiled}, nil
}

func (f *metaNameFormat) Format(name string) string {
	return strings.ReplaceAll(f.raw, "{name}", name)
}

func (f *metaNameFormat) Match(filename string) (string, bool) {
	if f == nil || f.compiledRegex == nil {
		return "", false
	}
	matches := f.compiledRegex.FindStringSubmatch(filename)
	if matches == nil {
		return "", false
	}
	base := matches[f.compiledRegex.SubexpIndex("name")]
	return base, true
}

func parseMetaFormat(s string) (metadataFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "simplejson":
		return metaFormatSimpleJSON, nil
	case "none":
		return metaFormatNone, nil
	default:
		return metaFormatNone, fmt.Errorf("unsupported metadata format: %s", s)
	}
}

func sanitizePath(path string) string {
	if path == "" {
		return "/"
	}
	return utils.FixAndCleanPath(path)
}

func joinPath(base, sub string) string {
	base = sanitizePath(base)
	sub = sanitizePath(sub)
	if utils.PathEqual(sub, "/") {
		return base
	}
	return utils.FixAndCleanPath(stdpath.Join(base, strings.TrimPrefix(sub, "/")))
}

func (d *Chunker) remoteMountPathFor(path string) string {
	return joinPath(d.RemotePath, path)
}

func (d *Chunker) remoteActualPathFor(path string) string {
	return joinPath(d.remoteActualRoot, path)
}

func (d *Chunker) collectGroup(ctx context.Context, dirPath string, remoteObjs []model.Obj) ([]model.Obj, map[string]*compositeGroup, error) {
	groups := make(map[string]*compositeGroup)
	var others []model.Obj

	for _, obj := range remoteObjs {
		if obj.IsDir() {
			others = append(others, convertRemoteObj(dirPath, obj))
			continue
		}
		name := obj.GetName()
		if base, idx, ok := d.nameFormat.Match(name); ok {
			group := ensureGroup(groups, dirPath, base)
			chunk := &chunkFile{
				Index:      idx,
				Name:       name,
				Size:       obj.GetSize(),
				ModTime:    obj.ModTime(),
				CreateTime: obj.CreateTime(),
				Hash:       obj.GetHash(),
				MountPath:  joinPath(d.remoteMountPathFor(dirPath), name),
				ActualPath: joinPath(d.remoteActualPathFor(dirPath), name),
			}
			group.Files = append(group.Files, chunk)
			continue
		}
		if base, ok := d.metaName.Match(name); ok {
			group := ensureGroup(groups, dirPath, base)
			group.Meta = &metaFile{
				Name:       name,
				MountPath:  joinPath(d.remoteMountPathFor(dirPath), name),
				ActualPath: joinPath(d.remoteActualPathFor(dirPath), name),
				Obj:        obj,
			}
			continue
		}
		others = append(others, convertRemoteObj(dirPath, obj))
	}

	var results []model.Obj
	for name, group := range groups {
		if len(group.Files) == 0 {
			// expose meta file if no chunks found
			if group.Meta != nil {
				others = append(others, convertRemoteObj(dirPath, group.Meta.Obj))
			}
			delete(groups, name)
			continue
		}
		sort.Slice(group.Files, func(i, j int) bool { return group.Files[i].Index < group.Files[j].Index })
		var offset int64
		for _, ch := range group.Files {
			ch.Offset = offset
			offset += ch.Size
		}
		group.TotalSize = offset
		obj := &model.Object{
			Name:     name,
			Size:     group.TotalSize,
			Modified: group.Files[len(group.Files)-1].ModTime,
			Ctime:    group.Files[0].CreateTime,
			IsFolder: false,
			Path:     joinPath(dirPath, name),
		}
		results = append(results, wrapChunkObject(obj, group))
	}
	results = append(results, others...)
	if d.metaName != nil {
		filtered := results[:0]
		for _, obj := range results {
			if _, ok := d.metaName.Match(obj.GetName()); ok {
				continue
			}
			filtered = append(filtered, obj)
		}
		results = filtered
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].GetName() < results[j].GetName()
	})
	return results, groups, nil
}

func ensureGroup(groups map[string]*compositeGroup, dirPath, base string) *compositeGroup {
	g, ok := groups[base]
	if !ok {
		g = &compositeGroup{DirPath: dirPath, BaseName: base}
		groups[base] = g
	}
	return g
}

func convertRemoteObj(dirPath string, obj model.Obj) model.Obj {
	if wrapper, ok := obj.(interface{ SetPath(string) }); ok {
		wrapper.SetPath(joinPath(dirPath, obj.GetName()))
		return obj
	}
	newObj := &model.Object{
		Name:     obj.GetName(),
		Size:     obj.GetSize(),
		Modified: obj.ModTime(),
		Ctime:    obj.CreateTime(),
		IsFolder: obj.IsDir(),
		HashInfo: obj.GetHash(),
		Path:     joinPath(dirPath, obj.GetName()),
	}
	if thumb, ok := model.GetThumb(obj); ok {
		return &model.ObjThumb{Object: *newObj, Thumbnail: model.Thumbnail{Thumbnail: thumb}}
	}
	return newObj
}

func (d *Chunker) readMetadata(ctx context.Context, group *compositeGroup) (*chunkMeta, error) {
	if group.Meta == nil {
		return nil, errs.ObjectNotFound
	}
	link, _, err := op.Link(ctx, d.remoteStorage, group.Meta.ActualPath, model.LinkArgs{Redirect: true})
	if err != nil {
		return nil, err
	}
	var reader io.ReadCloser
	switch {
	case link.RangeReadCloser != nil:
		reader, err = link.RangeReadCloser.RangeRead(ctx, http_range.Range{Start: 0, Length: -1})
	case link.MFile != nil:
		reader = io.NopCloser(io.NewSectionReader(link.MFile, 0, group.Meta.Obj.GetSize()))
	case link.URL != "":
		var rrc model.RangeReadCloserIF
		rrc, err = stream.GetRangeReadCloserFromLink(group.Meta.Obj.GetSize(), link)
		if err == nil {
			reader, err = rrc.RangeRead(ctx, http_range.Range{Start: 0, Length: -1})
			defer rrc.Close()
		}
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	switch d.metaFormat {
	case metaFormatSimpleJSON:
		var payload struct {
			Version  *int   `json:"ver"`
			Size     *int64 `json:"size"`
			ChunkNum *int   `json:"nchunks"`
			MD5      string `json:"md5"`
			SHA1     string `json:"sha1"`
			Txn      string `json:"txn"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, err
		}
		meta := &chunkMeta{
			MD5:  payload.MD5,
			SHA1: payload.SHA1,
			Txn:  payload.Txn,
		}
		if payload.Version != nil {
			meta.Version = *payload.Version
		}
		if payload.Size != nil {
			meta.Size = *payload.Size
		}
		if payload.ChunkNum != nil {
			meta.ChunkNum = *payload.ChunkNum
		}
		group.Meta.Meta = meta
		return meta, nil
	default:
		return nil, errs.NotImplement
	}
}

func (d *Chunker) marshalMetadata(size int64, chunkCount int, hashes utils.HashInfo) ([]byte, error) {
	switch d.metaFormat {
	case metaFormatNone:
		return nil, nil
	case metaFormatSimpleJSON:
		version := 2
		payload := struct {
			Version  *int   `json:"ver"`
			Size     *int64 `json:"size"`
			ChunkNum *int   `json:"nchunks"`
			MD5      string `json:"md5,omitempty"`
			SHA1     string `json:"sha1,omitempty"`
		}{
			Version:  &version,
			Size:     &size,
			ChunkNum: &chunkCount,
		}
		if md5 := hashes.GetHash(utils.MD5); md5 != "" {
			payload.MD5 = md5
		}
		if sha1 := hashes.GetHash(utils.SHA1); sha1 != "" {
			payload.SHA1 = sha1
		}
		return json.Marshal(&payload)
	default:
		return nil, errs.NotImplement
	}
}

func (d *Chunker) ensureMetaLoaded(ctx context.Context, group *compositeGroup) {
	if d.metaFormat == metaFormatNone || group == nil || group.Meta == nil || group.Meta.Meta != nil {
		return
	}
	if _, err := d.readMetadata(ctx, group); err != nil {
		group.Meta.Meta = nil
	}
}

func (d *Chunker) chunkRangeReader(ctx context.Context, group *compositeGroup, args model.LinkArgs) (model.RangeReadCloserIF, error) {
	segments := make([]*chunkFile, len(group.Files))
	copy(segments, group.Files)
	if len(segments) == 0 {
		return nil, errs.ObjectNotFound
	}
	total := group.TotalSize
	readerFunc := func(ctx context.Context, hr http_range.Range) (io.ReadCloser, error) {
		if hr.Start < 0 || hr.Start >= total {
			return io.NopCloser(strings.NewReader("")), nil
		}
		length := hr.Length
		if length < 0 || hr.Start+length > total {
			length = total - hr.Start
		}
		remaining := length
		pos := hr.Start
		var resources []struct {
			reader io.ReadCloser
			closer io.Closer
		}
		cleanup := func() {
			for _, res := range resources {
				if res.reader != nil {
					_ = res.reader.Close()
				}
				if res.closer != nil {
					_ = res.closer.Close()
				}
			}
		}
		for _, segment := range segments {
			segStart := segment.Offset
			segEnd := segStart + segment.Size
			if pos >= segEnd {
				continue
			}
			if pos < segStart {
				pos = segStart
			}
			offset := pos - segStart
			part := segment.Size - offset
			if part <= 0 {
				continue
			}
			if part > remaining {
				part = remaining
			}
			link, _, err := op.Link(ctx, d.remoteStorage, segment.ActualPath, args)
			if err != nil {
				cleanup()
				return nil, err
			}
			var chunkRRC model.RangeReadCloserIF
			if link.RangeReadCloser != nil {
				chunkRRC = link.RangeReadCloser
			} else if link.MFile != nil {
				chunkRRC = &model.RangeReadCloser{RangeReader: func(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
					if r.Length < 0 {
						r.Length = segment.Size - r.Start
					}
					return io.NopCloser(io.NewSectionReader(link.MFile, r.Start, r.Length)), nil
				}}
			} else if link.URL != "" {
				chunkRRC, err = stream.GetRangeReadCloserFromLink(segment.Size, link)
				if err != nil {
					cleanup()
					return nil, err
				}
			} else {
				cleanup()
				return nil, errs.NotSupport
			}
			reader, err := chunkRRC.RangeRead(ctx, http_range.Range{Start: offset, Length: part})
			if err != nil {
				cleanup()
				if chunkRRC != nil {
					_ = chunkRRC.Close()
				}
				return nil, err
			}
			resources = append(resources, struct {
				reader io.ReadCloser
				closer io.Closer
			}{reader: reader, closer: chunkRRC})
			remaining -= part
			pos += part
			if remaining <= 0 {
				break
			}
		}
		if len(resources) == 0 {
			cleanup()
			return io.NopCloser(strings.NewReader("")), nil
		}
		return &multiReadCloser{resources: resources}, nil
	}
	return &model.RangeReadCloser{RangeReader: readerFunc}, nil
}

type multiReadCloser struct {
	resources []struct {
		reader io.ReadCloser
		closer io.Closer
	}
	idx int
}

func (m *multiReadCloser) Read(p []byte) (int, error) {
	for m.idx < len(m.resources) {
		n, err := m.resources[m.idx].reader.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			_ = m.resources[m.idx].reader.Close()
			_ = m.resources[m.idx].closer.Close()
			m.idx++
			continue
		}
		return n, err
	}
	return 0, io.EOF
}

func (m *multiReadCloser) Close() error {
	var errsJoin error
	for _, res := range m.resources[m.idx:] {
		if res.reader != nil {
			if err := res.reader.Close(); err != nil && errsJoin == nil {
				errsJoin = err
			}
		}
		if res.closer != nil {
			if err := res.closer.Close(); err != nil && errsJoin == nil {
				errsJoin = err
			}
		}
	}
	m.resources = nil
	return errsJoin
}

func (m *multiReadCloser) RangeRead(ctx context.Context, r http_range.Range) (io.ReadCloser, error) {
	return nil, errs.NotSupport
}

var _ io.ReadCloser = (*multiReadCloser)(nil)
