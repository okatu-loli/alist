package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/fs"
)

// TestCleanTempDir_SkipsUploadPersistDir guards the ordering fix required for
// resumable uploads: fs.UploadPersistTempDirName holds temp files backing
// UploadTasks that InitTaskManager just recovered from the persisted DB
// state (possibly already running in another goroutine by the time
// CleanTempDir runs, since it's called last in InitTaskManager). Wiping that
// directory here would yank the content out from under them. Everything
// else in TempDir must still be cleaned as before.
func TestCleanTempDir_SkipsUploadPersistDir(t *testing.T) {
	oldConf := conf.Conf
	t.Cleanup(func() { conf.Conf = oldConf })
	conf.Conf = &conf.Config{TempDir: t.TempDir()}

	regularFile := filepath.Join(conf.Conf.TempDir, "file-transient")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed regular temp file: %v", err)
	}
	persistDir := filepath.Join(conf.Conf.TempDir, fs.UploadPersistTempDirName)
	if err := os.MkdirAll(persistDir, 0o777); err != nil {
		t.Fatalf("mkdir persist dir: %v", err)
	}
	persistedFile := filepath.Join(persistDir, "file-resumable")
	if err := os.WriteFile(persistedFile, []byte("resume me"), 0o644); err != nil {
		t.Fatalf("seed persisted temp file: %v", err)
	}

	CleanTempDir()

	if _, err := os.Stat(regularFile); !os.IsNotExist(err) {
		t.Errorf("regular temp file should have been wiped, stat err=%v", err)
	}
	if _, err := os.Stat(persistedFile); err != nil {
		t.Errorf("upload-persist file should have survived CleanTempDir: %v", err)
	}
}
