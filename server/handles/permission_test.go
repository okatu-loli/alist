package handles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	// _busy_timeout + a single pooled connection: TestLabelConcurrentDeleteVsApply
	// (labels_redteam_test.go) fires real concurrent goroutines at this shared
	// in-memory db (op.ApplyLabel × 20 racing op.DeleteLabel). Without a busy
	// timeout, two genuinely concurrent writers on SQLite's shared-cache mode
	// return "database table is locked" (SQLITE_LOCKED) immediately instead of
	// waiting -- confirmed by instrumenting that test to log the discarded
	// error instead of dropping it, which reproduced "DeleteLabel err: database
	// table is locked" on the exact run where the label survived (op.DeleteLabel
	// itself never got to run, not a logic bug in DeleteLabel/ApplyLabel).
	// _busy_timeout makes SQLite retry internally for up to 5s instead of
	// erroring out; pinning the pool to one physical connection (as
	// internal/db/agent_task_test.go and node_test.go already do for the same
	// class of test) serializes at database/sql instead of relying on SQLite's
	// own multi-connection locking, without serializing the goroutines
	// themselves -- they still race to reach the db, so the concurrency the
	// test means to exercise is unchanged.
	dB, err := gorm.Open(sqlite.Open("file:handles_test?mode=memory&cache=shared&_busy_timeout=5000"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	if sqlDB, err := dB.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

// UpdatePermission must merge onto the loaded row instead of zeroing fields
// the client omits from the request body. A partial body (id+effect only)
// must not wipe Name/Actions/Origin.
func TestUpdatePermissionMergesOntoLoadedRow(t *testing.T) {
	p := &model.Permission{
		Name:      "keep-name",
		Path:      "/pt",
		Actions:   model.Actions{model.ActRead, model.ActWrite},
		Effect:    model.EffectAllow,
		Recursive: true,
	}
	if err := op.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(fmt.Sprintf(`{"id":%d,"effect":"deny"}`, p.ID))
	req := httptest.NewRequest("POST", "/api/permission/update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	UpdatePermission(c)

	if w.Code != 200 {
		t.Fatalf("UpdatePermission returned %d: %s", w.Code, w.Body.String())
	}

	got, err := op.GetPermission(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effect != model.EffectDeny {
		t.Errorf("Effect = %q, want %q", got.Effect, model.EffectDeny)
	}
	if got.Name != "keep-name" {
		t.Errorf("Name = %q, want %q (must be preserved from omitted field)", got.Name, "keep-name")
	}
	if !got.Actions.Contains(model.ActRead) || !got.Actions.Contains(model.ActWrite) {
		t.Errorf("Actions = %v, want to still contain read+write (must be preserved from omitted field)", got.Actions)
	}
	if got.Origin != model.OriginManual {
		t.Errorf("Origin = %q, want %q (must never be client-editable)", got.Origin, model.OriginManual)
	}
}

// CreatePermission must force Origin to manual regardless of what the
// client sends: Origin is system-managed, scope rows are only created by
// role sync, so a client-labeled "scope" row must not be silently wiped by
// the next role sync (which regenerates scope-origin rows wholesale).
func TestCreatePermissionForcesOriginManual(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"name":"spoofed","path":"/spoof","effect":"allow","origin":"scope"}`)
	req := httptest.NewRequest("POST", "/api/permission/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	CreatePermission(c)

	if w.Code != 200 {
		t.Fatalf("CreatePermission returned %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data model.Permission `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(resp.Data.ID) })

	got, err := op.GetPermission(resp.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != model.OriginManual {
		t.Errorf("Origin = %q, want %q (client-sent \"scope\" must be ignored)", got.Origin, model.OriginManual)
	}
}

// CreatePermission must persist a client-supplied display_name, and GetPermission
// must read it back verbatim (round trip). display_name is purely cosmetic and
// distinct from Name/internal identifier.
func TestCreatePermissionDisplayNameRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"name":"internal-id","display_name":"Friendly Label","path":"/dn","effect":"allow"}`)
	req := httptest.NewRequest("POST", "/api/permission/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	CreatePermission(c)

	if w.Code != 200 {
		t.Fatalf("CreatePermission returned %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data model.Permission `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(resp.Data.ID) })

	if resp.Data.DisplayName != "Friendly Label" {
		t.Errorf("create response DisplayName = %q, want %q", resp.Data.DisplayName, "Friendly Label")
	}

	got, err := op.GetPermission(resp.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Friendly Label" {
		t.Errorf("GetPermission DisplayName = %q, want %q", got.DisplayName, "Friendly Label")
	}
	if got.Name != "internal-id" {
		t.Errorf("Name = %q, want %q (display_name must not overwrite the internal identifier)", got.Name, "internal-id")
	}
}

// CreatePermission with no display_name must persist an empty one (built-in
// and scope-origin permissions have no display_name; it must not error or
// default to something non-empty).
func TestCreatePermissionEmptyDisplayNameAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"name":"no-display","path":"/nd","effect":"allow"}`)
	req := httptest.NewRequest("POST", "/api/permission/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	CreatePermission(c)

	if w.Code != 200 {
		t.Fatalf("CreatePermission returned %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data model.Permission `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(resp.Data.ID) })

	got, err := op.GetPermission(resp.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "" {
		t.Errorf("DisplayName = %q, want empty", got.DisplayName)
	}
}

// UpdatePermission must persist a changed display_name and leave it readable
// via GetPermission (round trip), without disturbing Name.
func TestUpdatePermissionDisplayNameRoundTrip(t *testing.T) {
	p := &model.Permission{
		Name:        "keep-name",
		DisplayName: "Old Label",
		Path:        "/dnu",
		Actions:     model.Actions{model.ActRead},
		Effect:      model.EffectAllow,
		Recursive:   true,
	}
	if err := op.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(fmt.Sprintf(`{"id":%d,"display_name":"New Label"}`, p.ID))
	req := httptest.NewRequest("POST", "/api/permission/update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	UpdatePermission(c)

	if w.Code != 200 {
		t.Fatalf("UpdatePermission returned %d: %s", w.Code, w.Body.String())
	}

	got, err := op.GetPermission(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "New Label" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "New Label")
	}
	if got.Name != "keep-name" {
		t.Errorf("Name = %q, want %q (must be preserved from omitted field)", got.Name, "keep-name")
	}
}

// CreatePermission must reject an Effect that is neither "allow" nor "deny"
// with 400 before it ever reaches the op layer.
func TestCreatePermissionRejectsInvalidEffect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"name":"x","path":"/e","effect":"Deny","actions":["read"]}`)
	req := httptest.NewRequest("POST", "/api/permission/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	CreatePermission(c)

	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != 400 {
		t.Errorf("Code = %d, want 400 (body: %s)", resp.Code, w.Body.String())
	}
}

// UpdatePermission must reject an Effect that is neither "allow" nor "deny"
// with 400 before loading/merging onto the row.
func TestUpdatePermissionRejectsInvalidEffect(t *testing.T) {
	p := &model.Permission{
		Name:      "keep-name",
		Path:      "/pt2",
		Actions:   model.Actions{model.ActRead},
		Effect:    model.EffectAllow,
		Recursive: true,
	}
	if err := op.CreatePermission(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeletePermission(p.ID) })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(fmt.Sprintf(`{"id":%d,"effect":"Deny"}`, p.ID))
	req := httptest.NewRequest("POST", "/api/permission/update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	UpdatePermission(c)

	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != 400 {
		t.Errorf("Code = %d, want 400 (body: %s)", resp.Code, w.Body.String())
	}

	got, err := op.GetPermission(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effect != model.EffectAllow {
		t.Errorf("Effect = %q, want unchanged %q", got.Effect, model.EffectAllow)
	}
}
