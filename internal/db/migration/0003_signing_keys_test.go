package migration

import "testing"

func TestSigningKeysMigrationSeedsDownloadKey(t *testing.T) {
	d := openMem(t, "sign_mig")
	if err := New(d, testPrefix).Up(); err != nil {
		t.Fatalf("up: %v", err)
	}
	var count int64
	d.Table(testPrefix+"signing_keys").Where("purpose = ? AND active = ?", "download", true).Count(&count)
	if count != 1 {
		t.Fatalf("want exactly 1 active download key seeded, got %d", count)
	}
	var secret string
	d.Table(testPrefix+"signing_keys").Select("secret").Where("purpose = ?", "download").Scan(&secret)
	if len(secret) < 20 {
		t.Fatalf("seeded secret looks non-random: %q", secret)
	}
}
