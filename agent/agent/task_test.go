package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlistGo/alist-agent/proto"
)

// Agent-side copy-task execution tests (design W12-3b). jsonEnvelope
// (defined in client_test.go) is reused throughout — every fake server
// below answers exactly the server/common.Resp[T] shape the real handlers
// use, so these tests exercise the same wire contract the real server
// speaks (server/handles's own agent_task_test.go pins the server half; the
// full pair->task->heartbeat->execute->verify path is additionally covered
// end to end in server/handles, since only that package has the real
// handlers + a storage fixture to assert a genuine on-disk destination).

// --- FetchSrcLink ---

func TestFetchSrcLinkPostsCapTokenAndParsesResponse(t *testing.T) {
	var gotPath string
	var gotBody srcLinkReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		jsonEnvelope(w, 200, "success", srcLinkWire{
			URL:     "http://example.invalid/src",
			Headers: http.Header{"X-Test": {"1"}},
			Size:    42,
			Proxied: true,
		})
	}))
	defer srv.Close()

	link, err := FetchSrcLink(t.Context(), srv.URL, "task-1", "cap-tok")
	if err != nil {
		t.Fatalf("FetchSrcLink: %v", err)
	}
	if gotPath != "/api/v4/agent/task/task-1/src-link" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody.CapToken != "cap-tok" {
		t.Fatalf("cap_token not posted in body: %+v", gotBody)
	}
	if link.URL != "http://example.invalid/src" || link.Size != 42 || !link.Proxied {
		t.Fatalf("unexpected link: %+v", link)
	}
	if link.Headers.Get("X-Test") != "1" {
		t.Fatalf("headers not round-tripped: %+v", link.Headers)
	}
}

func TestFetchSrcLinkRejectionSurfacesServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonEnvelope(w, 401, "invalid or expired capability token", nil)
	}))
	defer srv.Close()

	if _, err := FetchSrcLink(t.Context(), srv.URL, "task-1", "bad-cap"); err == nil {
		t.Fatal("expected an error for a rejected cap token")
	}
}

// --- ReportProgress / ReportStatus ---

func TestReportProgressPostsExpectedFields(t *testing.T) {
	var got progressReq
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		jsonEnvelope(w, 200, "success", struct{}{})
	}))
	defer srv.Close()

	if err := ReportProgress(t.Context(), srv.URL, "task-9", "cap-tok", 55, "halfway"); err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if gotPath != "/api/v4/agent/task/task-9/progress" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if got.CapToken != "cap-tok" || got.Progress != 55 || got.Detail != "halfway" {
		t.Fatalf("unexpected body: %+v", got)
	}
}

func TestReportStatusPostsExpectedFields(t *testing.T) {
	var got statusReq
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		jsonEnvelope(w, 200, "success", struct{}{})
	}))
	defer srv.Close()

	if err := ReportStatus(t.Context(), srv.URL, "task-9", "cap-tok", proto.TaskStatusFailed, "boom"); err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if gotPath != "/api/v4/agent/task/task-9/status" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if got.CapToken != "cap-tok" || got.Status != proto.TaskStatusFailed || got.Detail != "boom" {
		t.Fatalf("unexpected body: %+v", got)
	}
}

// --- progressReader ---

// TestProgressReaderMonotonicAndThrottled pins that reported percentages
// strictly increase and that the reader does NOT report on every tiny
// Read() call — it is throttled by both a minimum pct step and a minimum
// interval, except the final (100%) report, which always fires so a caller
// can tell the read side of the transfer actually completed.
func TestProgressReaderMonotonicAndThrottled(t *testing.T) {
	data := strings.Repeat("a", 1000)
	var reports []int
	pr := newProgressReader(strings.NewReader(data), int64(len(data)), func(pct int) {
		reports = append(reports, pct)
	})

	// Read in small (37-byte) chunks so there are far more Read calls
	// (~27) than would be tolerable to report on every single one.
	buf := make([]byte, 37)
	reads := 0
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			reads++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected read error: %v", err)
		}
	}
	if reads < 20 {
		t.Fatalf("test setup: expected many small reads, got %d", reads)
	}
	if len(reports) == 0 {
		t.Fatal("expected at least one progress report")
	}
	if len(reports) >= reads {
		t.Fatalf("progress reporting was not throttled: %d reports for %d reads", len(reports), reads)
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] <= reports[i-1] {
			t.Fatalf("reported percentages must strictly increase, got %v", reports)
		}
	}
	if reports[len(reports)-1] != 100 {
		t.Fatalf("final report must be 100%%, got %v", reports)
	}
}

// --- downloadSrc: Content-Length resolution ---

// TestDownloadSrcKnownContentLengthStreamsDirectly pins the direct-stream
// path: when the GET response declares a real Content-Length, downloadSrc
// hands back the response body itself (no temp-file spool) with that
// length.
func TestDownloadSrcKnownContentLengthStreamsDirectly(t *testing.T) {
	body := "known-length source content"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Src-Auth"); got != "shh" {
			t.Errorf("expected src headers to be forwarded, got X-Src-Auth=%q", got)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	link := SrcLink{URL: srv.URL, Headers: http.Header{"X-Src-Auth": {"shh"}}, Size: int64(len(body))}
	rc, size, err := downloadSrc(t.Context(), link)
	if err != nil {
		t.Fatalf("downloadSrc: %v", err)
	}
	defer rc.Close()

	if _, isSpool := rc.(*tempSpoolFile); isSpool {
		t.Fatal("a known-Content-Length source must stream directly, not spool to a temp file")
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

// TestDownloadSrcChunkedSourceSpoolsToTemp pins the fallback path: a source
// that never declares a Content-Length (forced chunked, via an explicit
// Flush before any Write) and an src-link with an unknown (zero) Size must
// spool to a temp file to learn the length, and that file must be removed
// once the caller closes it.
func TestDownloadSrcChunkedSourceSpoolsToTemp(t *testing.T) {
	body := "chunked source content, length unknown up front"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // forces Transfer-Encoding: chunked, no Content-Length
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	link := SrcLink{URL: srv.URL, Size: 0} // Size unknown too — must fall through to spool
	rc, size, err := downloadSrc(t.Context(), link)
	if err != nil {
		t.Fatalf("downloadSrc: %v", err)
	}
	spool, ok := rc.(*tempSpoolFile)
	if !ok {
		t.Fatalf("expected a *tempSpoolFile, got %T", rc)
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q", got, body)
	}

	tmpPath := spool.File.Name()
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("temp spool file should still exist before Close: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temp spool file must be removed after Close, stat err = %v", err)
	}
}

// TestDownloadSrcUnknownTransportLengthFallsBackToLinkSize pins the middle
// case: the transport doesn't declare a length, but src-link's own Size is
// known and positive — that must be used directly, no spool.
func TestDownloadSrcUnknownTransportLengthFallsBackToLinkSize(t *testing.T) {
	body := "fallback to link.Size"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	link := SrcLink{URL: srv.URL, Size: int64(len(body))}
	rc, size, err := downloadSrc(t.Context(), link)
	if err != nil {
		t.Fatalf("downloadSrc: %v", err)
	}
	defer rc.Close()
	if _, isSpool := rc.(*tempSpoolFile); isSpool {
		t.Fatal("a known link.Size must stream directly, not spool")
	}
	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
}

// --- uploadContent: header-not-query cap token, exact bytes, progress ---

// TestUploadContentSendsCapTokenAsHeaderNotQuery pins the load-bearing
// security property: the capability token travels ONLY on the
// Agent-Cap-Token header, never in the URL/query string (which is far more
// likely to be captured verbatim in access/proxy logs) — matching
// server/handles.AgentTaskPutContent's header-only contract.
func TestUploadContentSendsCapTokenAsHeaderNotQuery(t *testing.T) {
	const payload = "upload-me"
	var gotHeader, gotQuery string
	var gotContentLength int64
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		gotHeader = r.Header.Get("Agent-Cap-Token")
		gotQuery = r.URL.RawQuery
		gotContentLength = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		jsonEnvelope(w, 200, "success", struct{}{})
	}))
	defer srv.Close()

	err := uploadContent(t.Context(), srv.URL, "task-1", "secret-cap-token", strings.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("uploadContent: %v", err)
	}
	if gotHeader != "secret-cap-token" {
		t.Fatalf("Agent-Cap-Token header = %q, want secret-cap-token", gotHeader)
	}
	if strings.Contains(gotQuery, "cap_token") || strings.Contains(gotQuery, "secret-cap-token") {
		t.Fatalf("cap token must never appear in the query string, got %q", gotQuery)
	}
	if gotContentLength != int64(len(payload)) {
		t.Fatalf("Content-Length = %d, want %d", gotContentLength, len(payload))
	}
	if string(gotBody) != payload {
		t.Fatalf("uploaded body = %q, want %q", gotBody, payload)
	}
}

func TestUploadContentReportsProgressThroughOnPct(t *testing.T) {
	payload := strings.Repeat("b", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		jsonEnvelope(w, 200, "success", struct{}{})
	}))
	defer srv.Close()

	var mu sync.Mutex
	var reports []int
	onPct := func(pct int) {
		mu.Lock()
		reports = append(reports, pct)
		mu.Unlock()
	}
	err := uploadContent(t.Context(), srv.URL, "task-1", "cap", strings.NewReader(payload), int64(len(payload)), onPct)
	if err != nil {
		t.Fatalf("uploadContent: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) == 0 {
		t.Fatal("expected at least one progress report")
	}
	if reports[len(reports)-1] != 100 {
		t.Fatalf("final report must be 100%%, got %v", reports)
	}
}

func TestUploadContentRejectionSurfacesServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		jsonEnvelope(w, 401, "invalid or expired capability token", nil)
	}))
	defer srv.Close()

	err := uploadContent(t.Context(), srv.URL, "task-1", "bad-cap", strings.NewReader("x"), 1, nil)
	if err == nil {
		t.Fatal("expected an error for a rejected upload")
	}
}

// --- RunCopyTask: end-to-end against fake control-plane + src handlers ---

// controlPlane fakes the four agent-facing task endpoints in one
// httptest.Server, recording exactly what RunCopyTask sends so these tests
// can assert on both the wire protocol and the ordering (running before any
// content moves, done only after the upload lands, a download failure
// producing a failed status with no upload attempt at all).
type controlPlane struct {
	mu         sync.Mutex
	statuses   []statusReq
	progresses []progressReq
	uploaded   []byte
	uploadHdr  http.Header
	uploadCL   int64
	srcLinkFn  func() srcLinkWire
	failUpload bool
}

func newControlPlane(srcLinkFn func() srcLinkWire) *controlPlane {
	return &controlPlane{srcLinkFn: srcLinkFn}
}

func (cp *controlPlane) server(t *testing.T, taskID string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/agent/task/"+taskID+"/src-link", func(w http.ResponseWriter, r *http.Request) {
		jsonEnvelope(w, 200, "success", cp.srcLinkFn())
	})
	mux.HandleFunc("/api/v4/agent/task/"+taskID+"/status", func(w http.ResponseWriter, r *http.Request) {
		var req statusReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		cp.mu.Lock()
		cp.statuses = append(cp.statuses, req)
		cp.mu.Unlock()
		jsonEnvelope(w, 200, "success", struct{}{})
	})
	mux.HandleFunc("/api/v4/agent/task/"+taskID+"/progress", func(w http.ResponseWriter, r *http.Request) {
		var req progressReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		cp.mu.Lock()
		cp.progresses = append(cp.progresses, req)
		cp.mu.Unlock()
		jsonEnvelope(w, 200, "success", struct{}{})
	})
	mux.HandleFunc("/api/v4/agent/task/"+taskID+"/content", func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.uploadHdr = r.Header.Clone()
		cp.uploadCL = r.ContentLength
		cp.mu.Unlock()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read uploaded body: %v", err)
		}
		cp.mu.Lock()
		cp.uploaded = body
		fail := cp.failUpload
		cp.mu.Unlock()
		if fail {
			jsonEnvelope(w, 500, "simulated upload failure", nil)
			return
		}
		jsonEnvelope(w, 200, "success", struct{}{})
	})
	return httptest.NewServer(mux)
}

func (cp *controlPlane) statusSeq(t *testing.T) []string {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var seq []string
	for _, s := range cp.statuses {
		seq = append(seq, s.Status)
	}
	return seq
}

// TestRunCopyTaskHappyPath drives RunCopyTask against a fake control plane
// plus a separate fake "source drive" server: it must report running, fetch
// the src-link, download the exact source bytes, upload them (with the cap
// token on the header, matching Content-Length), report progress, and
// finish with a done status — and the destination must have received
// EXACTLY the source's bytes.
func TestRunCopyTaskHappyPath(t *testing.T) {
	const taskID = "happy-task-1"
	const capToken = "happy-cap-token"
	srcBody := strings.Repeat("payload-bytes-", 200) // long enough to exercise >1 progress report

	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Drive-Auth"); got != "drive-secret" {
			t.Errorf("src headers not forwarded: X-Drive-Auth=%q", got)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(srcBody)))
		_, _ = w.Write([]byte(srcBody))
	}))
	defer srcSrv.Close()

	cp := newControlPlane(func() srcLinkWire {
		return srcLinkWire{
			URL:     srcSrv.URL,
			Headers: http.Header{"X-Drive-Auth": {"drive-secret"}},
			Size:    int64(len(srcBody)),
			Proxied: false,
		}
	})
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	err := RunCopyTask(t.Context(), ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/x/src", DstDir: "/y/dst"}, StaticToken(capToken))
	if err != nil {
		t.Fatalf("RunCopyTask: %v", err)
	}

	if string(cp.uploaded) != srcBody {
		t.Fatalf("destination did not receive exact source bytes (got %d bytes, want %d)", len(cp.uploaded), len(srcBody))
	}
	if cp.uploadHdr.Get("Agent-Cap-Token") != capToken {
		t.Fatalf("content upload cap token header = %q, want %q", cp.uploadHdr.Get("Agent-Cap-Token"), capToken)
	}
	if cp.uploadCL != int64(len(srcBody)) {
		t.Fatalf("content upload Content-Length = %d, want %d", cp.uploadCL, len(srcBody))
	}

	seq := cp.statusSeq(t)
	if len(seq) != 2 || seq[0] != proto.TaskStatusRunning || seq[1] != proto.TaskStatusDone {
		t.Fatalf("status sequence = %v, want [running done]", seq)
	}

	cp.mu.Lock()
	nProgress := len(cp.progresses)
	lastPct := 0
	if nProgress > 0 {
		lastPct = cp.progresses[nProgress-1].Progress
	}
	cp.mu.Unlock()
	if nProgress == 0 {
		t.Fatal("expected at least one progress report")
	}
	if lastPct != 100 {
		t.Fatalf("last reported progress = %d, want 100", lastPct)
	}
	for _, p := range cp.progresses {
		if p.CapToken != capToken {
			t.Fatalf("progress report missing/incorrect cap token: %+v", p)
		}
	}
}

// TestRunCopyTaskProxiedSrcLinkAlsoWorks pins that a proxied (server /d
// style) src-link is treated identically to a genuine direct-drive URL —
// RunCopyTask just GETs whatever URL/headers src-link handed back.
func TestRunCopyTaskProxiedSrcLinkAlsoWorks(t *testing.T) {
	const taskID = "proxied-task-1"
	const capToken = "proxied-cap-token"
	srcBody := "proxied-source-content"

	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(srcBody)))
		_, _ = w.Write([]byte(srcBody))
	}))
	defer srcSrv.Close()

	cp := newControlPlane(func() srcLinkWire {
		return srcLinkWire{URL: srcSrv.URL, Size: int64(len(srcBody)), Proxied: true}
	})
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	if err := RunCopyTask(t.Context(), ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/a", DstDir: "/b"}, StaticToken(capToken)); err != nil {
		t.Fatalf("RunCopyTask: %v", err)
	}
	if string(cp.uploaded) != srcBody {
		t.Fatalf("uploaded = %q, want %q", cp.uploaded, srcBody)
	}
}

// TestRunCopyTaskChunkedSourceSpoolsThenUploads exercises the full
// RunCopyTask path when the source is chunked/length-unknown AND src-link's
// own Size is unknown (0) — the spool-to-temp fallback inside downloadSrc
// must transparently produce a usable, correctly-sized upload.
func TestRunCopyTaskChunkedSourceSpoolsThenUploads(t *testing.T) {
	const taskID = "chunked-task-1"
	const capToken = "chunked-cap-token"
	srcBody := "chunked, length learned only by spooling to disk first"

	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(srcBody))
	}))
	defer srcSrv.Close()

	cp := newControlPlane(func() srcLinkWire {
		return srcLinkWire{URL: srcSrv.URL, Size: 0, Proxied: false}
	})
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	if err := RunCopyTask(t.Context(), ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/a", DstDir: "/b"}, StaticToken(capToken)); err != nil {
		t.Fatalf("RunCopyTask: %v", err)
	}
	if string(cp.uploaded) != srcBody {
		t.Fatalf("uploaded = %q, want %q", cp.uploaded, srcBody)
	}
	if cp.uploadCL != int64(len(srcBody)) {
		t.Fatalf("Content-Length = %d, want %d", cp.uploadCL, len(srcBody))
	}
}

// TestRunCopyTaskDownloadErrorReportsFailedStatus pins the failure path: a
// source download that fails (the src server 500s) must be reported as a
// failed status (never done, never a partial/no upload attempt) and
// RunCopyTask must return the original error.
func TestRunCopyTaskDownloadErrorReportsFailedStatus(t *testing.T) {
	const taskID = "fail-task-1"
	const capToken = "fail-cap-token"

	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "drive unavailable", http.StatusInternalServerError)
	}))
	defer srcSrv.Close()

	cp := newControlPlane(func() srcLinkWire {
		return srcLinkWire{URL: srcSrv.URL, Size: 10}
	})
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	err := RunCopyTask(t.Context(), ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/a", DstDir: "/b"}, StaticToken(capToken))
	if err == nil {
		t.Fatal("expected RunCopyTask to return the download error")
	}

	seq := cp.statusSeq(t)
	if len(seq) != 2 || seq[0] != proto.TaskStatusRunning || seq[1] != proto.TaskStatusFailed {
		t.Fatalf("status sequence = %v, want [running failed]", seq)
	}
	cp.mu.Lock()
	uploaded := cp.uploaded
	cp.mu.Unlock()
	if uploaded != nil {
		t.Fatalf("no upload should have been attempted after a download failure, got %d bytes", len(uploaded))
	}
}

// TestRunCopyTaskUploadErrorReportsFailedStatusWithoutMaskingError pins
// that an upload-side failure ALSO reports a failed status (best-effort —
// its own error must never mask the real cause) and that RunCopyTask still
// returns the underlying upload error, not some generic wrapper of the
// (successful) status report.
func TestRunCopyTaskUploadErrorReportsFailedStatusWithoutMaskingError(t *testing.T) {
	const taskID = "upload-fail-task-1"
	const capToken = "upload-fail-cap-token"
	srcBody := "will fail to upload"

	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(srcBody)))
		_, _ = w.Write([]byte(srcBody))
	}))
	defer srcSrv.Close()

	cp := newControlPlane(func() srcLinkWire {
		return srcLinkWire{URL: srcSrv.URL, Size: int64(len(srcBody))}
	})
	cp.failUpload = true
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	err := RunCopyTask(t.Context(), ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/a", DstDir: "/b"}, StaticToken(capToken))
	if err == nil {
		t.Fatal("expected RunCopyTask to return the upload error")
	}
	if !strings.Contains(err.Error(), "simulated upload failure") {
		t.Fatalf("original upload error must not be masked, got: %v", err)
	}

	seq := cp.statusSeq(t)
	if len(seq) != 2 || seq[0] != proto.TaskStatusRunning || seq[1] != proto.TaskStatusFailed {
		t.Fatalf("status sequence = %v, want [running failed]", seq)
	}
}

// TestRunCopyTaskRespectsContextCancellation pins that a cancelled ctx
// unwinds RunCopyTask (rather than, say, hanging or silently ignoring
// cancellation) while STILL managing to best-effort report a failed status
// on its own, independent context (see RunCopyTask's doc on
// reportFailureTimeout).
func TestRunCopyTaskRespectsContextCancellation(t *testing.T) {
	const taskID = "cancel-task-1"
	const capToken = "cancel-cap-token"

	blockSrc := make(chan struct{})
	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blockSrc // never unblocks within the test's timeout
	}))
	defer srcSrv.Close()
	defer close(blockSrc)

	cp := newControlPlane(func() srcLinkWire {
		return srcLinkWire{URL: srcSrv.URL, Size: 10}
	})
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	err := RunCopyTask(ctx, ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/a", DstDir: "/b"}, StaticToken(capToken))
	if err == nil {
		t.Fatal("expected RunCopyTask to return an error when ctx is cancelled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a context.DeadlineExceeded-wrapped error, got: %v", err)
	}

	seq := cp.statusSeq(t)
	if len(seq) != 2 || seq[0] != proto.TaskStatusRunning || seq[1] != proto.TaskStatusFailed {
		t.Fatalf("status sequence = %v, want [running failed] even after cancellation", seq)
	}
}

// TestRunCopyTaskUsesRenewedTokenForNextCall pins the token-provider
// threading (design W12-4 (A)): RunCopyTask must consult tokenFn again
// before EVERY subsequent server call, not just once at the start — so a
// renewal that lands mid-run (the heartbeat loop swapping in a fresh cap
// token while a copy is still in flight, because the copy has outlived one
// token's TTL) is picked up by the very next call RunCopyTask makes.
func TestRunCopyTaskUsesRenewedTokenForNextCall(t *testing.T) {
	const taskID = "renew-task-1"
	srcBody := "renewed-token-body"

	var mu sync.Mutex
	current := "token-v1"
	tokenFn := func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	renew := func(tok string) {
		mu.Lock()
		defer mu.Unlock()
		current = tok
	}

	srcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(srcBody)))
		_, _ = w.Write([]byte(srcBody))
	}))
	defer srcSrv.Close()

	cp := newControlPlane(func() srcLinkWire {
		// Simulate a heartbeat renewal landing exactly as src-link is
		// fetched: RunCopyTask's INITIAL running-report (already sent by
		// this point) used token-v1, but everything AFTER this — download,
		// progress reports, upload, the final status — must pick up
		// token-v2 on its own next call, without RunCopyTask needing to be
		// told explicitly.
		renew("token-v2")
		return srcLinkWire{URL: srcSrv.URL, Size: int64(len(srcBody)), Proxied: true}
	})
	ctlSrv := cp.server(t, taskID)
	defer ctlSrv.Close()

	if err := RunCopyTask(t.Context(), ctlSrv.URL, Task{TaskID: taskID, Type: "copy", SrcPath: "/a", DstDir: "/b"}, tokenFn); err != nil {
		t.Fatalf("RunCopyTask: %v", err)
	}

	if got := cp.uploadHdr.Get("Agent-Cap-Token"); got != "token-v2" {
		t.Fatalf("content upload must use the RENEWED token, got %q", got)
	}

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.statuses) != 2 {
		t.Fatalf("expected 2 status reports, got %d", len(cp.statuses))
	}
	if cp.statuses[0].CapToken != "token-v1" {
		t.Fatalf("the INITIAL running report (sent before any renewal) must have used token-v1, got %q", cp.statuses[0].CapToken)
	}
	if cp.statuses[1].CapToken != "token-v2" {
		t.Fatalf("the final done report (sent after renewal) must use token-v2, got %q", cp.statuses[1].CapToken)
	}
	for _, p := range cp.progresses {
		if p.CapToken != "token-v2" {
			t.Fatalf("progress reports after renewal must use token-v2, got %+v", p)
		}
	}
}

// TestScrubURLErrorStripsSignedURL: a download failure wrapping a *url.Error
// must not leak the (possibly presigned/credential-bearing) source URL into the
// error string that ends up in agent logs egressing to the admin UI.
func TestScrubURLError(t *testing.T) {
	ue := &url.Error{
		Op:  "Get",
		URL: "https://cdn.example.com/file.bin?sign=SECRETSIG123&uid=42",
		Err: errors.New("connection refused"),
	}
	got := scrubURLError(ue).Error()
	if strings.Contains(got, "SECRETSIG123") || strings.Contains(got, "sign=") || strings.Contains(got, "file.bin") {
		t.Fatalf("scrubbed error still leaks the URL/query: %q", got)
	}
	if !strings.Contains(got, "cdn.example.com") || !strings.Contains(got, "connection refused") {
		t.Fatalf("scrubbed error should keep host + underlying cause, got %q", got)
	}
	// A non-url.Error passes through unchanged.
	plain := errors.New("some other failure")
	if scrubURLError(plain).Error() != "some other failure" {
		t.Fatalf("non-url.Error must pass through unchanged")
	}
}
