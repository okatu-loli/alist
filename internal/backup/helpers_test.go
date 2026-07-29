package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/backup"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
)

func roleByName(t *testing.T, name string) *model.Role {
	t.Helper()
	var r model.Role
	if err := db.GetDb().Where("name = ?", name).First(&r).Error; err != nil {
		t.Fatalf("role %s: %v", name, err)
	}
	return &r
}

func storageByMount(t *testing.T, mount string) *model.Storage {
	t.Helper()
	var s model.Storage
	if err := db.GetDb().Where("mount_path = ?", mount).First(&s).Error; err != nil {
		t.Fatalf("storage %s: %v", mount, err)
	}
	return &s
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func sameDomainDiff(a, b backup.DomainDiff) bool {
	eq := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	return eq(a.Create, b.Create) && eq(a.Update, b.Update) && eq(a.Delete, b.Delete)
}

// craftArchive builds a structurally valid tar.gz with the given schema_version
// and empty domain payloads — enough to exercise the manifest validation path
// without depending on Write (which always stamps the real LatestID).
func craftArchive(t *testing.T, schemaVersion string) []byte {
	t.Helper()
	manifest := backup.Manifest{
		FormatVersion: backup.FormatVersion,
		SchemaVersion: schemaVersion,
		CreatedAt:     time.Now(),
		Domains:       map[string]int{},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(b))}); err != nil {
			t.Fatalf("hdr %s: %v", name, err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatalf("body %s: %v", name, err)
		}
	}
	write("manifest.json", manifest)
	for _, d := range []string{
		backup.DomainSettings, backup.DomainStorages, backup.DomainMetas,
		backup.DomainRoles, backup.DomainPermissions, backup.DomainUsers,
		backup.DomainUserRoles, backup.DomainRolePermissions,
	} {
		write(d+".json", []any{})
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
