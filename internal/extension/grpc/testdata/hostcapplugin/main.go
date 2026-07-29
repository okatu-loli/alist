// Command hostcapplugin is the F0 reverse-bridge integration fixture: a plugin
// subprocess that borrows Host capabilities (FS/Store/HTTP) over the real
// plugin->host gRPC bridge and reports the outcome, so the host integration test
// can assert the security invariants end to end (Authorize on FS, namespace
// isolation on Store, allowlist + Grant on HTTP).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	ext "github.com/AlistGo/alist-ext"
	"github.com/AlistGo/alist-ext/grpcplugin"
)

func main() {
	grpcplugin.ServeHostCapProbe(probe)
}

// probe exercises one borrowed capability per call. It uses the empty Principal
// (the plugin's own bound principal), so the host authorizes against the grant
// the test wired for this plugin.
func probe(ctx context.Context, host ext.Host, op, arg, arg2 string) (string, error) {
	switch op {
	case "fs.list":
		objs, err := host.FS().List(ctx, "", ext.ObjRef{Anchor: arg}, ext.ListArgs{})
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(objs))
		for _, o := range objs {
			names = append(names, o.Name)
		}
		sort.Strings(names)
		return strings.Join(names, ","), nil
	case "fs.get":
		o, err := host.FS().Get(ctx, "", ext.ObjRef{Anchor: arg})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s:%d", o.Name, o.Size), nil
	case "fs.mkdir":
		o, err := host.FS().Mkdir(ctx, "", ext.ObjRef{Anchor: arg}, arg2)
		if err != nil {
			return "", err
		}
		return o.Name, nil
	case "store.set":
		if err := host.Store().KV().Set(ctx, arg, []byte(arg2)); err != nil {
			return "", err
		}
		return "ok", nil
	case "store.get":
		v, ok, err := host.Store().KV().Get(ctx, arg)
		if err != nil {
			return "", err
		}
		if !ok {
			return "<missing>", nil
		}
		return string(v), nil
	case "secret.set":
		if err := host.Store().Secrets().Set(ctx, arg, []byte(arg2)); err != nil {
			return "", err
		}
		return "ok", nil
	case "secret.get":
		v, ok, err := host.Store().Secrets().Get(ctx, arg)
		if err != nil {
			return "", err
		}
		if !ok {
			return "<missing>", nil
		}
		return string(v), nil
	case "table.insert":
		// arg=table name, arg2=JSON row.
		row := map[string]any{}
		if err := json.Unmarshal([]byte(arg2), &row); err != nil {
			return "", err
		}
		id, err := host.Store().Table(arg).Insert(ctx, row)
		if err != nil {
			return "", err
		}
		return id, nil
	case "table.get":
		// arg=table name, arg2=row id.
		row, ok, err := host.Store().Table(arg).Get(ctx, arg2)
		if err != nil {
			return "", err
		}
		if !ok {
			return "<missing>", nil
		}
		data, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		return string(data), nil
	case "table.list":
		// arg=table name. Returns "id1:json1,id2:json2" sorted by id.
		rows, err := host.Store().Table(arg).List(ctx)
		if err != nil {
			return "", err
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		parts := make([]string, 0, len(rows))
		for _, r := range rows {
			data, merr := json.Marshal(r.Row)
			if merr != nil {
				return "", merr
			}
			parts = append(parts, r.ID+":"+string(data))
		}
		return strings.Join(parts, ","), nil
	case "http.get":
		resp, err := host.HTTP().Do(ctx, &ext.HTTPRequest{Method: "GET", URL: arg})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d:%s", resp.Status, string(resp.Body)), nil
	case "auth.login":
		// arg=username, arg2=password
		prin, err := host.Auth().Login(ctx, arg, arg2)
		if err != nil {
			return "", err
		}
		return string(prin), nil
	case "auth.verifytoken.fs.list":
		// arg=token, arg2=dir path. Proves the Principal VerifyToken issues over
		// the wire is usable with host.FS (not just non-empty).
		prin, err := host.Auth().VerifyToken(ctx, arg)
		if err != nil {
			return "", err
		}
		objs, err := host.FS().List(ctx, prin, ext.ObjRef{Anchor: arg2}, ext.ListArgs{})
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(objs))
		for _, o := range objs {
			names = append(names, o.Name)
		}
		sort.Strings(names)
		return strings.Join(names, ","), nil
	case "sign.link":
		// arg=path to sign a download link for.
		tok, err := host.Sign().SignLink(ctx, "", ext.ObjRef{Anchor: arg}, ext.SignScopeDownload, 0)
		if err != nil {
			return "", err
		}
		return tok, nil
	case "sign.verify":
		// arg=token from a prior sign.link.
		ref, scope, err := host.Sign().VerifyLink(ctx, arg)
		if err != nil {
			return "", err
		}
		return ref.Anchor + ":" + string(scope), nil
	case "config.save":
		// arg=the config JSON bytes to save.
		if err := host.SaveConfig(ctx, []byte(arg)); err != nil {
			return "", err
		}
		return "ok", nil
	case "capabilities":
		apis, err := host.Capabilities(ctx)
		if err != nil {
			return "", err
		}
		sort.Strings(apis)
		return strings.Join(apis, ","), nil
	case "fs.put":
		// arg=destination dir path, arg2=file content.
		up := &byteUploadStream{name: "put-by-plugin.txt", data: []byte(arg2)}
		o, err := host.FS().Put(ctx, "", ext.ObjRef{Anchor: arg}, up)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s:%d", o.Name, o.Size), nil
	default:
		return "", ext.NewError(ext.ErrCodeNotSupport, "unknown op "+op)
	}
}

// byteUploadStream is a minimal ext.UploadStream a real plugin (which already
// holds the bytes, unlike the driver-side pull model) would hand host.FS().Put.
type byteUploadStream struct {
	name string
	data []byte
}

func (b *byteUploadStream) Info() ext.UploadInfo {
	return ext.UploadInfo{Name: b.name, Size: int64(len(b.data))}
}

func (b *byteUploadStream) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	return copy(p, b.data[off:]), nil
}
