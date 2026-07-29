package _123Open

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// TestDriverRegistered verifies the driver is discoverable end-to-end via the
// extension registry, which is exactly what GET /api/v4/admin/drivers returns.
func TestDriverRegistered(t *testing.T) {
	infoMap := op.GetDriverInfoMap()
	info, ok := infoMap[config.Name]
	if !ok {
		t.Fatalf("driver %q not registered; available: %v", config.Name, keysOf(infoMap))
	}
	// config form must expose the Addition fields (e.g. client_id)
	var hasClientID bool
	for _, item := range info.Additional {
		if item.Name == "client_id" {
			hasClientID = true
		}
	}
	if !hasClientID {
		t.Fatalf("expected client_id field in config form, got %+v", info.Additional)
	}
}

func keysOf[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// fakeServer models just enough of the 123 open platform API for the driver
// tests. Individual handlers can be overridden per-test.
type fakeServer struct {
	t *testing.T

	server *httptest.Server

	tokenCalls    int32
	completeCalls int32
	createCalls   int32

	mu             sync.Mutex
	receivedSlices map[int64][]byte // sliceNo -> bytes
	singleUpload   []byte

	// overridable behavior
	listHandler   func(w http.ResponseWriter, r *http.Request)
	tokenHandler  func(w http.ResponseWriter, r *http.Request)
	createHandler func(w http.ResponseWriter, r *http.Request)
}

func newFakeServer(t *testing.T) *fakeServer {
	fs := &fakeServer{t: t, receivedSlices: map[int64][]byte{}}
	mux := http.NewServeMux()

	mux.HandleFunc(ApiToken, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.tokenCalls, 1)
		if fs.tokenHandler != nil {
			fs.tokenHandler(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]any{
				"accessToken": "test-token",
				"expiredAt":   time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		})
	})

	mux.HandleFunc(ApiFileList, func(w http.ResponseWriter, r *http.Request) {
		if fs.listHandler != nil {
			fs.listHandler(w, r)
			return
		}
		fs.defaultList(w, r)
	})

	mux.HandleFunc(ApiGetDirectLink, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data":    map[string]any{"url": "https://cdn.example.com/download/file.bin"},
		})
	})

	mux.HandleFunc(ApiMakeDir, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data":    map[string]any{"dirID": 99},
		})
	})

	mux.HandleFunc(ApiRemove, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "ok"})
	})

	mux.HandleFunc(ApiMove, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "ok"})
	})

	mux.HandleFunc(ApiRename, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "ok"})
	})

	// upload: create -> (single | slice) -> complete
	mux.HandleFunc(ApiCreateUploadURL, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.createCalls, 1)
		if fs.createHandler != nil {
			fs.createHandler(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]any{
				"fileId":      0,
				"reuse":       false,
				"preuploadId": "pre-123",
				"sliceSize":   8,
				"servers":     []string{fs.server.URL},
			},
		})
	})

	mux.HandleFunc(ApiSingleUploadURL, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		fs.mu.Lock()
		fs.singleUpload = b
		fs.mu.Unlock()
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data":    map[string]any{"fileID": 555, "completed": true},
		})
	})

	mux.HandleFunc(ApiUploadSliceURL, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sliceNo := parseInt(r.FormValue("sliceNo"))
		f, _, err := r.FormFile("slice")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		fs.mu.Lock()
		fs.receivedSlices[sliceNo] = b
		fs.mu.Unlock()
		writeJSON(w, map[string]any{"code": 0, "message": "ok"})
	})

	mux.HandleFunc(ApiUploadCompleteURL, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.completeCalls, 1)
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data":    map[string]any{"completed": true, "fileID": 777},
		})
	})

	fs.server = httptest.NewServer(mux)
	t.Cleanup(fs.server.Close)
	return fs
}

// defaultList returns two pages: lastFileId 0 -> page with one real + one
// trashed file and lastFileId=10; lastFileId 10 -> one file and lastFileId=-1.
func (fs *fakeServer) defaultList(w http.ResponseWriter, r *http.Request) {
	last := r.URL.Query().Get("lastFileId")
	switch last {
	case "0":
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]any{
				"lastFileId": 10,
				"fileList": []map[string]any{
					{"fileId": 1, "filename": "a.txt", "type": 0, "size": 100, "trashed": 0},
					{"fileId": 2, "filename": "trashed.txt", "type": 0, "size": 50, "trashed": 1},
				},
			},
		})
	default: // "10"
		writeJSON(w, map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]any{
				"lastFileId": -1,
				"fileList": []map[string]any{
					{"fileId": 3, "filename": "b.txt", "type": 0, "size": 200, "trashed": 0},
				},
			},
		})
	}
}

func (fs *fakeServer) newDriver() *Open123 {
	d := &Open123{
		apiBase: fs.server.URL,
		Addition: Addition{
			ClientID:     "cid",
			ClientSecret: "csecret",
		},
	}
	if err := d.Init(context.Background()); err != nil {
		fs.t.Fatalf("Init failed: %v", err)
	}
	return d
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func TestTokenFetchAndRefresh(t *testing.T) {
	fs := newFakeServer(t)
	d := fs.newDriver() // Init already fetched once
	if got := atomic.LoadInt32(&fs.tokenCalls); got != 1 {
		t.Fatalf("expected 1 token call after Init, got %d", got)
	}

	// cached: no new fetch
	if _, err := d.tm.getToken(); err != nil {
		t.Fatalf("getToken: %v", err)
	}
	if got := atomic.LoadInt32(&fs.tokenCalls); got != 1 {
		t.Fatalf("expected token still cached (1 call), got %d", got)
	}

	// force expiry -> refetch
	d.tm.mu.Lock()
	d.tm.expireTime = time.Now()
	d.tm.mu.Unlock()
	if _, err := d.tm.getToken(); err != nil {
		t.Fatalf("getToken after expiry: %v", err)
	}
	if got := atomic.LoadInt32(&fs.tokenCalls); got != 2 {
		t.Fatalf("expected refetch (2 calls), got %d", got)
	}
}

func TestListPaginationMerge(t *testing.T) {
	fs := newFakeServer(t)
	d := fs.newDriver()

	objs, err := d.List(context.Background(), &model.Object{ID: "0"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 non-trashed files across 2 pages, got %d", len(objs))
	}
	names := []string{objs[0].GetName(), objs[1].GetName()}
	if names[0] != "a.txt" || names[1] != "b.txt" {
		t.Fatalf("unexpected merged names: %v", names)
	}
}

func TestLinkReturnsMockedURL(t *testing.T) {
	fs := newFakeServer(t)
	d := fs.newDriver()

	link, err := d.Link(context.Background(), File{FileId: 1, Type: 0}, model.LinkArgs{})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if link.URL != "https://cdn.example.com/download/file.bin" {
		t.Fatalf("unexpected link URL: %s", link.URL)
	}
}

func TestApiErrorSurfacesAsError(t *testing.T) {
	fs := newFakeServer(t)
	fs.listHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 1, "message": "boom"})
	}
	d := fs.newDriver()

	_, err := d.List(context.Background(), &model.Object{ID: "0"}, model.ListArgs{})
	if err == nil {
		t.Fatalf("expected error from API error code, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error message to include API message, got: %v", err)
	}
}

func TestPutSingleUpload(t *testing.T) {
	fs := newFakeServer(t)
	d := fs.newDriver()

	content := []byte("hello single upload path")
	file := newTestStream(t, "single.bin", content)

	err := d.Put(context.Background(), &model.Object{ID: "0"}, file, func(float64) {})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	fs.mu.Lock()
	got := fs.singleUpload
	fs.mu.Unlock()
	if string(got) != string(content) {
		t.Fatalf("single upload bytes mismatch: got %q want %q", got, content)
	}
}

func TestPutSlicedUpload(t *testing.T) {
	fs := newFakeServer(t)
	d := fs.newDriver()

	// Force the sliced create->slice->complete path with a tiny file by
	// lowering the internal single-upload threshold. sliceSize is 8 (create
	// fixture), so 20 bytes => 3 slices.
	old := singleUploadThreshold
	singleUploadThreshold = 4
	defer func() { singleUploadThreshold = old }()

	content := []byte("0123456789abcdefghij") // 20 bytes -> slices of 8,8,4
	file := newTestStream(t, "sliced.bin", content)

	err := d.Put(context.Background(), &model.Object{ID: "0"}, file, func(float64) {})
	if err != nil {
		t.Fatalf("Put sliced: %v", err)
	}
	if got := atomic.LoadInt32(&fs.completeCalls); got != 1 {
		t.Fatalf("expected upload_complete called once, got %d", got)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.receivedSlices) != 3 {
		t.Fatalf("expected 3 slices received, got %d", len(fs.receivedSlices))
	}
	var reassembled []byte
	for i := int64(1); i <= 3; i++ {
		reassembled = append(reassembled, fs.receivedSlices[i]...)
	}
	if string(reassembled) != string(content) {
		t.Fatalf("reassembled slices mismatch: got %q want %q", reassembled, content)
	}
}

// newTestStream builds a tmpfile-backed FileStream whose MD5 hash is set so the
// driver skips re-hashing, and whose RangeRead can seek to any slice offset.
func newTestStream(t *testing.T, name string, content []byte) model.FileStreamer {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "123open-*")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := tmp.Write(content); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek temp: %v", err)
	}
	sum := md5.Sum(content)
	fs := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     int64(len(content)),
			Modified: time.Now(),
			HashInfo: utils.NewHashInfo(utils.MD5, hex.EncodeToString(sum[:])),
		},
		Mimetype: "application/octet-stream",
	}
	fs.SetTmpFile(tmp)
	return fs
}
