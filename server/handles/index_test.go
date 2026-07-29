package handles

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/gin-gonic/gin"

	"github.com/alist-org/alist/v3/internal/search"
)

// Index management handler tests (子项目 A, wave A3). These pin the request-layer
// contract: the running-guard gates Build/Stop/Clear, and GetProgress surfaces the
// persisted ext.IndexProgress JSON. The heavy walk itself is covered in
// internal/search; here we drive only the thin gin handlers.

func driveIndex(h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", nil)
	h(c)
	return w
}

// holdBuild installs the build guard so search.Running() reports true, returning
// the channel StopIndex would signal. Cleanup releases it.
func holdBuild(t *testing.T) chan struct{} {
	t.Helper()
	held := make(chan struct{}, 1)
	if !search.Quit.CompareAndSwap(nil, &held) {
		t.Fatal("could not install build guard (already running?)")
	}
	t.Cleanup(func() { search.Quit.Store(nil) })
	return held
}

func TestBuildIndexHandlerRejectsWhenRunning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	holdBuild(t)
	w := driveIndex(BuildIndex)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("BuildIndex while running must be 400, got %d: %s", got, w.Body.String())
	}
}

func TestClearIndexHandlerRejectsWhenRunning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	holdBuild(t)
	w := driveIndex(ClearIndex)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("ClearIndex while running must be 400, got %d: %s", got, w.Body.String())
	}
}

func TestStopIndexHandlerRejectsWhenNotRunning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if search.Running() {
		t.Fatal("precondition: no build should be running")
	}
	w := driveIndex(StopIndex)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("StopIndex while idle must be 400, got %d: %s", got, w.Body.String())
	}
}

func TestStopIndexHandlerSignalsWhenRunning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	held := holdBuild(t)
	w := driveIndex(StopIndex)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("StopIndex while running must be 200, got %d: %s", got, w.Body.String())
	}
	select {
	case <-held:
		// StopIndex delivered the abort signal.
	default:
		t.Fatal("StopIndex must send into the guard channel")
	}
}

// driveUpdate drives UpdateIndex with a JSON body (it binds a request, unlike
// the bodyless Build/Stop/Clear handlers).
func driveUpdate(body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/x", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	UpdateIndex(c)
	return w
}

// TestUpdateIndexHandlerRejectsWhenRunning: an in-flight build blocks a targeted
// update (400), same running-guard as Build/Clear.
func TestUpdateIndexHandlerRejectsWhenRunning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	holdBuild(t)
	w := driveUpdate(`{"paths":["/"]}`)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("UpdateIndex while running must be 400, got %d: %s", got, w.Body.String())
	}
}

// TestUpdateIndexHandlerRejectsNonAutoUpdate: with no active (or a non-AutoUpdate)
// backend, incremental update is unsupported and rejected (400).
func TestUpdateIndexHandlerRejectsNonAutoUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if search.Running() {
		t.Fatal("precondition: no build should be running")
	}
	if search.Config().AutoUpdate {
		t.Skip("an AutoUpdate backend is active; this test needs none")
	}
	w := driveUpdate(`{"paths":["/"]}`)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("UpdateIndex without an AutoUpdate backend must be 400, got %d: %s", got, w.Body.String())
	}
}

func TestGetProgressHandlerReturnsJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := search.WriteProgress(&ext.IndexProgress{ObjCount: 7, IsDone: true}); err != nil {
		t.Fatalf("seed progress: %v", err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)
	GetProgress(c)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("GetProgress must be 200, got %d: %s", got, w.Body.String())
	}
	var env struct {
		Data ext.IndexProgress `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse progress resp: %v", err)
	}
	if env.Data.ObjCount != 7 || !env.Data.IsDone {
		t.Fatalf("progress data = %+v, want ObjCount 7 IsDone true", env.Data)
	}
}
