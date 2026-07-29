package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/cmd/flags"
	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/bootstrap/data"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/net"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// recordingTransport 记录并拒绝一切出站 HTTP 请求。
// 隐私基线（rollout Step 2a）：默认配置 + 常规操作下，服务端主动联网必须为零。
type recordingTransport struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req.Method+" "+req.URL.String())
	r.mu.Unlock()
	return nil, fmt.Errorf("outbound request blocked by privacy baseline test: %s", req.URL)
}

func (r *recordingTransport) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestNoOutboundRequestsByDefault(t *testing.T) {
	rec := &recordingTransport{}

	// Save original values of all globals that will be modified
	oldDefault := http.DefaultTransport
	oldDataDir := flags.DataDir
	oldMigrateMode := flags.MigrateMode
	oldStoragesLoaded := conf.StoragesLoaded
	oldGinMode := gin.Mode()

	http.DefaultTransport = rec
	t.Cleanup(func() {
		http.DefaultTransport = oldDefault
		flags.DataDir = oldDataDir
		flags.MigrateMode = oldMigrateMode
		conf.StoragesLoaded = oldStoragesLoaded
		gin.SetMode(oldGinMode)
	})

	// 最小真实启动：临时数据目录 + 默认配置 + sqlite + 自动迁移
	flags.DataDir = t.TempDir()
	flags.MigrateMode = "auto" // 测试不经 cobra，零值会被 InitDB 拒绝
	bootstrap.InitConfig()
	bootstrap.InitDB()
	data.InitData()

	// InitConfig 里 base.InitClient() 已构建四个共享 client（各自持有独立 Transport），
	// 逐一换成记录器——驱动侧的出站都走它们
	oldHttpClientTransport := base.HttpClient.Transport
	oldRestyTransport := base.RestyClient.GetClient().Transport
	oldNoRedirectTransport := base.NoRedirectClient.GetClient().Transport
	oldNetHttpClientTransport := net.HttpClient().Transport
	t.Cleanup(func() {
		base.HttpClient.Transport = oldHttpClientTransport
		base.RestyClient.GetClient().Transport = oldRestyTransport
		base.NoRedirectClient.GetClient().Transport = oldNoRedirectTransport
		net.HttpClient().Transport = oldNetHttpClientTransport
	})

	base.HttpClient.Transport = rec
	base.RestyClient.SetTransport(rec)
	base.NoRedirectClient.SetTransport(rec)
	net.HttpClient().Transport = rec

	// 挂一个本地存储，让 fs 操作有真实对象
	storageDir := t.TempDir()
	if err := os.WriteFile(storageDir+"/hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: "/local",
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, storageDir),
	}); err != nil {
		t.Fatalf("create storage: %v", err)
	}
	// production startup sets this once bootstrap.LoadStorages()'s async load
	// finishes; here the storage is created synchronously above, so mirror that.
	conf.StoragesLoaded = true

	gin.SetMode(gin.TestMode)
	e := gin.New()
	Init(e)

	// 给 admin 设已知密码并登录
	admin, err := op.GetAdmin()
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	admin.SetPassword("baseline-test")
	if err := op.UpdateUser(admin); err != nil {
		t.Fatalf("update admin: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"username": admin.Username, "password": "baseline-test"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	e.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	var loginResp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &loginResp)
	token := loginResp.Data.Token
	if token == "" {
		t.Fatalf("no token in login response: %s", w.Body.String())
	}

	do := func(method, path string, payload any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		if payload != nil {
			_ = json.NewEncoder(&buf).Encode(payload)
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Authorization", token)
		req.Header.Set("Content-Type", "application/json")
		e.ServeHTTP(w, req)
		return w
	}

	// 常规操作：列目录、看文件详情
	// Coverage boundaries for privacy baseline test:
	// Intercepted (Transport blocked): Regular operations (fs/list, fs/get) use Local driver and walk through
	//   the five shared HTTP clients (base.HttpClient, base.RestyClient, base.NoRedirectClient, net.HttpClient,
	//   http.DefaultTransport). All outbound requests via these paths must be zero by default.
	// Not intercepted / Out of scope: Proxy download and streaming-source paths (net.RequestHttp -> net.HttpClient())
	//   have Transport intercepted but are not operationally touched by fs/list or fs/get against Local storage.

	if w := do("POST", "/api/fs/list", map[string]any{"path": "/local"}); w.Code != 200 {
		t.Fatalf("fs/list failed: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/fs/get", map[string]any{"path": "/local/hello.txt"}); w.Code != 200 {
		t.Fatalf("fs/get failed: %d %s", w.Code, w.Body.String())
	}

	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("privacy baseline violated: %d outbound request(s) during default boot + basic ops:\n%s",
			len(calls), joinLines(calls))
	}
}

func joinLines(ss []string) string {
	out := ""
	for _, s := range ss {
		out += "  " + s + "\n"
	}
	return out
}
