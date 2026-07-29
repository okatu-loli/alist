package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/audit"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

// boot spins a throwaway sqlite database so the writer has somewhere to flush.
func boot(t *testing.T) {
	t.Helper()
	oldDataDir := flags.DataDir
	oldMode := flags.MigrateMode
	t.Cleanup(func() {
		flags.DataDir = oldDataDir
		flags.MigrateMode = oldMode
	})
	flags.DataDir = t.TempDir()
	flags.MigrateMode = "auto"
	bootstrap.InitConfig()
	bootstrap.InitDB()
}

func drain(t *testing.T) {
	t.Helper()
	audit.Stop() // Stop flushes everything buffered before returning
}

func TestWriterRoundTrip(t *testing.T) {
	boot(t)
	audit.Start()

	ctx := context.WithValue(
		context.WithValue(context.Background(), audit.CtxIPKey, "203.0.113.9"),
		audit.CtxProtocolKey, "http")

	u := &model.User{Username: "alice"}
	u.ID = 7
	audit.Decision(ctx, u, "fs.read", "/local/secret.txt", false)
	audit.Event(ctx, 0, "mallory", "auth.login", "", false)
	drain(t)

	logs, total, err := db.GetAuditLogs(db.AuditLogFilter{}, 1, 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != 2 || len(logs) != 2 {
		t.Fatalf("expected 2 entries, got total=%d len=%d", total, len(logs))
	}
	// newest first; both written near-simultaneously so match by action
	byAction := map[string]model.AuditLog{}
	for _, l := range logs {
		byAction[l.Action] = l
	}
	dec := byAction["fs.read"]
	if dec.ActorUID != 7 || dec.Actor != "alice" || dec.Result != "deny" ||
		dec.TargetRef != "/local/secret.txt" || dec.IP != "203.0.113.9" || dec.Protocol != "http" {
		t.Errorf("decision row wrong: %+v", dec)
	}
	ev := byAction["auth.login"]
	if ev.ActorUID != 0 || ev.Actor != "mallory" || ev.Result != "failure" {
		t.Errorf("event row wrong: %+v", ev)
	}
}

func TestQueryFilters(t *testing.T) {
	boot(t)
	audit.Start()
	ctx := context.Background()
	u := &model.User{Username: "bob"}
	u.ID = 3
	audit.Decision(ctx, u, "fs.read", "/a", true)
	audit.Decision(ctx, u, "fs.write", "/a", false)
	audit.Event(ctx, 9, "carol", "role.update", "role:1", true)
	drain(t)

	// action prefix
	_, total, err := db.GetAuditLogs(db.AuditLogFilter{ActionPrefix: "fs."}, 1, 10)
	if err != nil || total != 2 {
		t.Fatalf("fs. prefix: total=%d err=%v", total, err)
	}
	// result
	_, total, _ = db.GetAuditLogs(db.AuditLogFilter{Result: "deny"}, 1, 10)
	if total != 1 {
		t.Fatalf("deny filter: total=%d", total)
	}
	// actor
	_, total, _ = db.GetAuditLogs(db.AuditLogFilter{ActorUID: 9}, 1, 10)
	if total != 1 {
		t.Fatalf("actor filter: total=%d", total)
	}
	// time window excluding everything
	_, total, _ = db.GetAuditLogs(db.AuditLogFilter{To: time.Now().Add(-time.Hour)}, 1, 10)
	if total != 0 {
		t.Fatalf("time filter: total=%d", total)
	}
}

func TestPrune(t *testing.T) {
	boot(t)
	old := model.AuditLog{Action: "fs.read", Result: "allow", Ts: time.Now().AddDate(0, 0, -120)}
	fresh := model.AuditLog{Action: "fs.read", Result: "allow", Ts: time.Now()}
	if err := db.CreateAuditLogs([]model.AuditLog{old, fresh}); err != nil {
		t.Fatal(err)
	}
	n, err := db.PruneAuditLogs(time.Now().AddDate(0, 0, -audit.RetentionDays))
	if err != nil || n != 1 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	_, total, _ := db.GetAuditLogs(db.AuditLogFilter{}, 1, 10)
	if total != 1 {
		t.Fatalf("expected 1 surviving entry, got %d", total)
	}
}
