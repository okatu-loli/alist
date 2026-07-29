package migration

import "testing"

func TestTokensMigrationCreatesTable(t *testing.T) {
	d := openMem(t, "tokens_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := d.Exec("INSERT INTO "+testPrefix+"tokens (jti, user_id, session_id, kind, expires_at, revoked, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"jti1", 1, "sess1", "access", int64(1<<62), false, int64(1)).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	var count int64
	d.Table(testPrefix+"tokens").Where("jti = ?", "jti1").Count(&count)
	if count != 1 {
		t.Fatalf("want 1 row, got %d", count)
	}
}
