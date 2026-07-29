package grpcproto

import (
	"net/http"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

func TestObjRoundTrip(t *testing.T) {
	o := ext.Obj{
		Name:     "a.txt",
		Size:     42,
		Modified: 1700,
		Created:  1600,
		IsDir:    false,
		ID:       "id-1",
		Path:     "/a.txt",
		Hashes:   map[string]string{"md5": "deadbeef"},
		Thumb:    "t",
		URL:      "u",
	}
	got := ObjFromProto(ObjToProto(o))
	if got.Name != o.Name || got.Size != o.Size || got.Modified != o.Modified ||
		got.Created != o.Created || got.IsDir != o.IsDir || got.ID != o.ID ||
		got.Path != o.Path || got.Thumb != o.Thumb || got.URL != o.URL {
		t.Fatalf("obj scalar mismatch: %+v vs %+v", got, o)
	}
	if got.Hashes["md5"] != "deadbeef" {
		t.Fatalf("obj hashes lost: %+v", got.Hashes)
	}
}

func TestLinkURLRoundTrip(t *testing.T) {
	l := &ext.Link{
		ByteSource:   ext.ByteSourceURL,
		URL:          "https://x/y",
		Header:       http.Header{"X-A": []string{"1", "2"}, "X-B": []string{"3"}},
		RangeSupport: true,
		Expiration:   3600,
		IPCacheKey:   true,
		Concurrency:  4,
		PartSize:     1024,
	}
	got := LinkFromProto(LinkToProto(l))
	if got.ByteSource != l.ByteSource || got.URL != l.URL || got.RangeSupport != l.RangeSupport ||
		got.Expiration != l.Expiration || got.IPCacheKey != l.IPCacheKey ||
		got.Concurrency != l.Concurrency || got.PartSize != l.PartSize {
		t.Fatalf("link scalar mismatch: %+v vs %+v", got, l)
	}
	if len(got.Header["X-A"]) != 2 || got.Header["X-A"][0] != "1" || got.Header["X-A"][1] != "2" ||
		got.Header.Get("X-B") != "3" {
		t.Fatalf("link header mismatch: %v", got.Header)
	}
}

func TestLinkStreamRoundTrip(t *testing.T) {
	l := &ext.Link{ByteSource: ext.ByteSourceStream, StreamID: "s#1", RangeSupport: true}
	got := LinkFromProto(LinkToProto(l))
	if got.ByteSource != ext.ByteSourceStream || got.StreamID != "s#1" || !got.RangeSupport {
		t.Fatalf("stream link mismatch: %+v", got)
	}
}

func TestErrorRoundTrip(t *testing.T) {
	// coded error -> preserves code so the host can map it back to a sentinel
	e := ErrorFromProto(ErrorToProto(ext.NewError(ext.ErrCodeNotImplement, "nope")))
	if code, ok := ext.CodeOf(e); !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("coded error lost: %v (code %q ok %v)", e, code, ok)
	}
	// nil error -> nil proto -> nil error
	if ErrorFromProto(ErrorToProto(nil)) != nil {
		t.Fatal("nil error should round-trip to nil")
	}
	// plain error -> carries message, no code
	pe := ErrorFromProto(ErrorToProto(errString("boom")))
	if pe == nil || pe.Error() != "boom" {
		t.Fatalf("plain error mismatch: %v", pe)
	}
	if _, ok := ext.CodeOf(pe); ok {
		t.Fatalf("plain error should have no code")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestUploadInfoRoundTrip(t *testing.T) {
	i := ext.UploadInfo{
		Name:     "f",
		Size:     9,
		Mimetype: "text/plain",
		Hashes:   map[string]string{"sha1": "x"},
		Exist:    &ext.Obj{Name: "f", Size: 9},
	}
	got := UploadInfoFromProto(UploadInfoToProto(i))
	if got.Name != i.Name || got.Size != i.Size || got.Mimetype != i.Mimetype ||
		got.Hashes["sha1"] != "x" || got.Exist == nil || got.Exist.Name != "f" {
		t.Fatalf("upload info mismatch: %+v", got)
	}
}
