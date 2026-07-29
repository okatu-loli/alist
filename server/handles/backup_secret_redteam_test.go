package handles

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/crypto"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/gin-gonic/gin"
)

// TestRedteam_BackupAuditLogNeverContainsPasswordOrKey pins Minor 8 (G4 red
// team: "does the audit record carry the key"). audit.Event's own signature
// (internal/audit/audit.go) has no free-form payload field at all — only
// actor/action/targetRef/success — and every backup.* call site
// (server/handles/backup.go) passes just the archive file name as
// targetRef, never req.Password or any secret material. This test confirms
// that structural property holds end to end for CreateBackup with
// include_secrets_key set (the highest-stakes G4 backup call, since it's
// the one that actually reads and packages the shared secrets key): the
// audit row for it must contain neither the request's password nor the raw
// secrets key bytes.
func TestRedteam_BackupAuditLogNeverContainsPasswordOrKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const password = "backup-audit-redteam-password-1"

	liveKey, err := crypto.LoadOrCreateKey(extension.PluginsDir(), crypto.SecretsKeyFile)
	if err != nil {
		t.Fatalf("resolve live secrets key: %v", err)
	}

	audit.Start()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ctxkey.User, guardPluginAdmin())
	body := `{"password":"` + password + `","include_secrets_key":true}`
	c.Request = httptest.NewRequest("POST", "/api/v4/admin/backup", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	CreateBackup(c)
	audit.Stop() // flushes everything buffered before returning

	if w.Code != 200 {
		t.Fatalf("CreateBackup: want 200, got %d: %s", w.Code, w.Body.String())
	}

	logs, _, err := db.GetAuditLogs(db.AuditLogFilter{ActionPrefix: "backup.create"}, 1, 50)
	if err != nil {
		t.Fatalf("GetAuditLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected at least one backup.create audit row")
	}
	for _, l := range logs {
		if strings.Contains(l.TargetRef, password) || strings.Contains(l.Action, password) || strings.Contains(l.Actor, password) {
			t.Fatalf("REGRESSION (Minor 8): backup password leaked into an audit log row: %+v", l)
		}
		if strings.Contains(l.TargetRef, string(liveKey)) || strings.Contains(l.Action, string(liveKey)) {
			t.Fatalf("REGRESSION (Minor 8): the raw secrets key leaked into an audit log row: %+v", l)
		}
	}
}
