package aria2

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:aria2_tool_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

// setAria2Settings points aria2_uri (and optionally aria2_secret) at the mock
// server so the tool's lazy connect builds a client against it. Cleared on
// cleanup.
func setAria2Settings(t *testing.T, uri, secret string) {
	t.Helper()
	for _, it := range []model.SettingItem{
		{Key: conf.Aria2Uri, Value: uri, Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD},
		{Key: conf.Aria2Secret, Value: secret, Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD, Flag: model.PRIVATE},
	} {
		item := it
		if err := op.SaveSettingItem(&item); err != nil {
			t.Fatalf("seed %s: %v", it.Key, err)
		}
	}
	t.Cleanup(func() {
		op.SettingCacheUpdate()
		_ = db.SaveSettingItem(&model.SettingItem{Key: conf.Aria2Uri, Value: "", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD})
		_ = db.SaveSettingItem(&model.SettingItem{Key: conf.Aria2Secret, Value: "", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD})
		op.SettingCacheUpdate()
	})
}

// countingHandler returns an http handler that echoes m.results[method] (or a
// JSON-RPC error for an unknown method) AND records, under mu, how many times
// each method was called — so a test can assert NO file-deleting method fires.
func countingHandler(t *testing.T, m *mockAria2, mu *sync.Mutex, called map[string]int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		called[req.Method]++
		mu.Unlock()
		if result, ok := m.results[req.Method]; ok {
			_, _ = w.Write([]byte(`{"result":` + result + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"code":1,"message":"unknown method ` + req.Method + `"}}`))
	})
}

func TestToolSatisfiesInterface(t *testing.T) {
	var _ ext.OfflineDownloadTool = (*Tool)(nil)
}

func TestToolConfigName(t *testing.T) {
	tool := &Tool{}
	if tool.Config().Name != "aria2" {
		t.Fatalf("Config().Name = %q, want aria2", tool.Config().Name)
	}
	if tool.Config().Name != ToolName {
		t.Fatalf("Config().Name %q != ToolName %q", tool.Config().Name, ToolName)
	}
}

func TestToolManifest(t *testing.T) {
	tool := &Tool{}
	m := tool.Manifest()
	if m.ID != ExtensionID {
		t.Fatalf("manifest ID = %q, want ExtensionID %q", m.ID, ExtensionID)
	}
	if ExtensionID != "builtin.offline_download.aria2" {
		t.Fatalf("ExtensionID = %q, want builtin.offline_download.aria2", ExtensionID)
	}
	if len(m.Kinds) != 1 || m.Kinds[0] != ext.KindOfflineDownloadTool {
		t.Fatalf("manifest kinds = %v", m.Kinds)
	}
}

func TestToolAddURL(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.getVersion"] = `{"version":"1.36.0"}`
	m.results["aria2.addUri"] = `"gid-777"`
	setAria2Settings(t, m.srv.URL, "")

	tool := &Tool{}
	gid, err := tool.AddURL(context.Background(), ext.AddArgs{URL: "http://example.com/a.iso", TempDir: "/tmp/dl"})
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if gid != "gid-777" {
		t.Fatalf("gid = %q, want gid-777", gid)
	}
	if m.lastReq.Method != "aria2.addUri" {
		t.Fatalf("last method = %q", m.lastReq.Method)
	}
}

func TestToolStatusMapping(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.getVersion"] = `{"version":"1.36.0"}`
	setAria2Settings(t, m.srv.URL, "")
	tool := &Tool{}
	ctx := context.Background()

	// active: totalLength/completedLength → TotalBytes/Progress, followedBy → NewGID.
	m.results["aria2.tellStatus"] = `{"status":"active","totalLength":"2000","completedLength":"500","followedBy":["gid-next"]}`
	st, err := tool.Status(ctx, "g")
	if err != nil {
		t.Fatalf("Status active: %v", err)
	}
	if st.TotalBytes != 2000 {
		t.Fatalf("TotalBytes = %d, want 2000", st.TotalBytes)
	}
	if st.Progress != 25 {
		t.Fatalf("Progress = %v, want 25", st.Progress)
	}
	if st.NewGID != "gid-next" {
		t.Fatalf("NewGID = %q, want gid-next", st.NewGID)
	}
	if st.Completed {
		t.Fatal("active must not be Completed")
	}
	if st.Err != "" {
		t.Fatalf("active Err = %v, want empty", st.Err)
	}

	// complete → Completed true.
	m.results["aria2.tellStatus"] = `{"status":"complete","totalLength":"2000","completedLength":"2000"}`
	st, err = tool.Status(ctx, "g")
	if err != nil {
		t.Fatalf("Status complete: %v", err)
	}
	if !st.Completed {
		t.Fatal("status complete must set Completed")
	}
	if st.Progress != 100 {
		t.Fatalf("Progress = %v, want 100", st.Progress)
	}

	// error → Err set from errorMessage.
	m.results["aria2.tellStatus"] = `{"status":"error","totalLength":"0","completedLength":"0","errorMessage":"boom"}`
	st, err = tool.Status(ctx, "g")
	if err != nil {
		t.Fatalf("Status error: %v", err)
	}
	if st.Err != "boom" {
		t.Fatalf("error status Err = %v, want boom", st.Err)
	}
	// div-by-zero guard: total 0 → Progress 0, not NaN.
	if st.Progress != 0 {
		t.Fatalf("zero-total Progress = %v, want 0", st.Progress)
	}
}

// TestToolRemoveNoFileDelete proves Remove calls aria2.remove AND
// aria2.removeDownloadResult and NEVER any file-deleting variant (aria2 has no
// deleteFiles rpc; a purge* method would be the mistake to catch).
func TestToolRemoveNoFileDelete(t *testing.T) {
	var (
		mu     sync.Mutex
		called = map[string]int{}
	)
	m := newMockAria2(t)
	m.results["aria2.getVersion"] = `{"version":"1.36.0"}`
	m.results["aria2.remove"] = `"g"`
	m.results["aria2.removeDownloadResult"] = `"OK"`
	// Reinstall the handler with a counting wrapper.
	m.srv.Config.Handler = countingHandler(t, m, &mu, called)

	setAria2Settings(t, m.srv.URL, "")
	tool := &Tool{}
	if err := tool.Remove(context.Background(), "g"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if called["aria2.remove"] == 0 {
		t.Fatal("Remove must call aria2.remove")
	}
	if called["aria2.removeDownloadResult"] == 0 {
		t.Fatal("Remove must (best-effort) call aria2.removeDownloadResult")
	}
	for method := range called {
		if method == "aria2.purgeDownloadResult" || method == "aria2.forceRemove" {
			t.Fatalf("Remove invoked a purge/force-remove method %q — must not delete files", method)
		}
	}
}
