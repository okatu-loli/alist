package handles

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// B7 per-request tool picker endpoint: ListOfflineDownloadTools returns the
// tools a caller may pick (SimpleHttp first + every enabled registered tool)
// plus the default the dialog should preselect (the offline_download_tool
// setting, normalized to SimpleHttp when unavailable).

// parseToolList unwraps a common.Resp[{tools,default}] response body.
func parseToolList(t *testing.T, w *httptest.ResponseRecorder) (tools []string, def string) {
	t.Helper()
	var env struct {
		Data struct {
			Tools   []string `json:"tools"`
			Default string   `json:"default"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse tool list: %v (%s)", err, w.Body.String())
	}
	return env.Data.Tools, env.Data.Default
}

// TestListOfflineDownloadToolsDefaultSimpleHttp: with only the base and the
// setting on SimpleHttp, the list is exactly [SimpleHttp] and the default is
// SimpleHttp.
func TestListOfflineDownloadToolsDefaultSimpleHttp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setOfflineDownloadTool(t, "SimpleHttp")

	u := nonAdminUser(t)
	w := driveJSON(u, "GET", "", ListOfflineDownloadTools)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("expected 200, got %d: %s", got, w.Body.String())
	}
	tools, def := parseToolList(t, w)
	if len(tools) == 0 || tools[0] != "SimpleHttp" {
		t.Fatalf("tools = %v, want SimpleHttp first", tools)
	}
	if def != "SimpleHttp" {
		t.Fatalf("default = %q, want SimpleHttp", def)
	}
}

// TestListOfflineDownloadToolsIncludesEnabledDefault: a registered+enabled tool
// selected as the setting default appears in the list and is returned as the
// default.
func TestListOfflineDownloadToolsIncludesEnabledDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registerB6Tool(t, "listtool")
	setOfflineDownloadTool(t, "listtool")

	u := nonAdminUser(t)
	w := driveJSON(u, "GET", "", ListOfflineDownloadTools)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("expected 200, got %d: %s", got, w.Body.String())
	}
	tools, def := parseToolList(t, w)
	if tools[0] != "SimpleHttp" {
		t.Fatalf("tools = %v, want SimpleHttp first", tools)
	}
	found := false
	for _, tn := range tools {
		if tn == "listtool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools = %v, want the enabled tool listtool present", tools)
	}
	if def != "listtool" {
		t.Fatalf("default = %q, want listtool", def)
	}
}

// TestListOfflineDownloadToolsStaleDefaultNormalizes: when the setting names a
// tool that is not available, the endpoint still lists what IS available and
// normalizes the default back to SimpleHttp (so the client never preselects a
// missing option).
func TestListOfflineDownloadToolsStaleDefaultNormalizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setOfflineDownloadTool(t, "ghosttool") // never registered

	u := nonAdminUser(t)
	w := driveJSON(u, "GET", "", ListOfflineDownloadTools)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("expected 200, got %d: %s", got, w.Body.String())
	}
	tools, def := parseToolList(t, w)
	if def != "SimpleHttp" {
		t.Fatalf("default = %q, want SimpleHttp (stale setting normalized)", def)
	}
	for _, tn := range tools {
		if tn == "ghosttool" {
			t.Fatalf("tools = %v, must not list the unregistered ghosttool", tools)
		}
	}
}
