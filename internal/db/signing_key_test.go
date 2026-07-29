package db

import (
	"testing"

	"github.com/alist-org/alist/v3/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupSigningKeyTestDB(t *testing.T) {
	t.Helper()
	dsn := "file:signkey_test_" + t.Name() + "?mode=memory&cache=shared"
	dB, err := gorm.Open(sqlite.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := dB.AutoMigrate(&model.SigningKey{}); err != nil {
		t.Fatal(err)
	}
	old := db
	db = dB
	t.Cleanup(func() { db = old })
}

func TestSigningKeyCRUD(t *testing.T) {
	setupSigningKeyTestDB(t)
	k := &model.SigningKey{Purpose: model.SignPurposeDownload, KID: "kid1", Secret: "s1", Active: true, CreatedAt: 1}
	if err := CreateSigningKey(k); err != nil {
		t.Fatal(err)
	}
	got, err := GetActiveSigningKey(model.SignPurposeDownload)
	if err != nil || got.KID != "kid1" {
		t.Fatalf("active: %v %v", got, err)
	}
	// rotate: deactivate then add new active
	if err := DeactivateSigningKeys(model.SignPurposeDownload); err != nil {
		t.Fatal(err)
	}
	k2 := &model.SigningKey{Purpose: model.SignPurposeDownload, KID: "kid2", Secret: "s2", Active: true, CreatedAt: 2}
	if err := CreateSigningKey(k2); err != nil {
		t.Fatal(err)
	}
	all, err := GetSigningKeys(model.SignPurposeDownload)
	if err != nil || len(all) != 2 {
		t.Fatalf("all keys: %d %v", len(all), err)
	}
	act, _ := GetActiveSigningKey(model.SignPurposeDownload)
	if act.KID != "kid2" {
		t.Fatalf("active after rotate: %s", act.KID)
	}
	// grace key still fetchable, then removable
	if err := DeleteSigningKey("kid1"); err != nil {
		t.Fatal(err)
	}
	all, _ = GetSigningKeys(model.SignPurposeDownload)
	if len(all) != 1 {
		t.Fatalf("after delete: %d", len(all))
	}
}

func TestRotateSigningKeyTx(t *testing.T) {
	setupSigningKeyTestDB(t)
	k1 := &model.SigningKey{Purpose: model.SignPurposeDownload, KID: "kid1", Secret: "s1", Active: true, CreatedAt: 1}
	if err := CreateSigningKey(k1); err != nil {
		t.Fatal(err)
	}
	k2 := &model.SigningKey{Purpose: model.SignPurposeDownload, KID: "kid2", Secret: "s2", Active: true, CreatedAt: 2}
	if err := RotateSigningKeyTx(model.SignPurposeDownload, k2); err != nil {
		t.Fatal(err)
	}
	all, err := GetSigningKeys(model.SignPurposeDownload)
	if err != nil || len(all) != 2 {
		t.Fatalf("all keys: %d %v", len(all), err)
	}
	activeCount := 0
	for _, k := range all {
		if k.Active {
			activeCount++
			if k.KID != "kid2" {
				t.Fatalf("wrong key left active: %s", k.KID)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active key, got %d", activeCount)
	}
	got, err := GetActiveSigningKey(model.SignPurposeDownload)
	if err != nil || got.KID != "kid2" {
		t.Fatalf("active after rotate: %v %v", got, err)
	}
}

func TestDeleteSigningKeysTx(t *testing.T) {
	setupSigningKeyTestDB(t)
	k1 := &model.SigningKey{Purpose: model.SignPurposeDownload, KID: "kid1", Secret: "s1", Active: false, CreatedAt: 1}
	k2 := &model.SigningKey{Purpose: model.SignPurposeDownload, KID: "kid2", Secret: "s2", Active: true, CreatedAt: 2}
	if err := CreateSigningKey(k1); err != nil {
		t.Fatal(err)
	}
	if err := CreateSigningKey(k2); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSigningKeysTx([]string{"kid1"}); err != nil {
		t.Fatal(err)
	}
	all, err := GetSigningKeys(model.SignPurposeDownload)
	if err != nil || len(all) != 1 || all[0].KID != "kid2" {
		t.Fatalf("after delete: %+v %v", all, err)
	}
	// no-op on empty
	if err := DeleteSigningKeysTx(nil); err != nil {
		t.Fatal(err)
	}
	all, err = GetSigningKeys(model.SignPurposeDownload)
	if err != nil || len(all) != 1 {
		t.Fatalf("after no-op delete: %+v %v", all, err)
	}
}
