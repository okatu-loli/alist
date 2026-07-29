package fs

import (
	"strings"
	"testing"

	"github.com/alist-org/alist/v3/internal/i18n"
)

// TestCopyTask_NameRendersPerLocale checks that a copy task exposes its name as
// an i18n key + params so the API layer can render it in the viewer's locale.
func TestCopyTask_NameRendersPerLocale(t *testing.T) {
	ct := &CopyTask{
		SrcObjPath:   "/a/b.txt",
		DstDirPath:   "/c",
		SrcStorageMp: "/src",
		DstStorageMp: "/dst",
	}
	if key := ct.GetNameKey(); key != "task.copy" {
		t.Fatalf("GetNameKey() = %q, want task.copy", key)
	}
	en := i18n.T("en", ct.GetNameKey(), ct.GetNameArgs()...)
	if en != "copy [/src](/a/b.txt) to [/dst](/c)" {
		t.Errorf("en name = %q", en)
	}
	zh := i18n.T("zh-CN", ct.GetNameKey(), ct.GetNameArgs()...)
	if !strings.Contains(zh, "复制") || !strings.Contains(zh, "/a/b.txt") {
		t.Errorf("zh name = %q, want it localized with the paths", zh)
	}
	if zh == en {
		t.Errorf("name did not localize: en and zh both %q", en)
	}
	// GetName() keeps returning the English rendering for logging/back-compat.
	if got := ct.GetName(); got != en {
		t.Errorf("GetName() = %q, want English rendering %q", got, en)
	}
}

// TestCopyTask_StatusRendersPerLocale checks the copy status is keyed too.
func TestCopyTask_StatusRendersPerLocale(t *testing.T) {
	ct := &CopyTask{}
	ct.Status = copyStatusGettingSrc
	if key := ct.GetStatusKey(); key != copyStatusGettingSrc {
		t.Fatalf("GetStatusKey() = %q, want %q", key, copyStatusGettingSrc)
	}
	en := i18n.T("en", ct.GetStatusKey(), ct.GetStatusArgs()...)
	zh := i18n.T("zh-CN", ct.GetStatusKey(), ct.GetStatusArgs()...)
	if en == "" || zh == "" || en == zh {
		t.Errorf("status did not localize: en=%q zh=%q", en, zh)
	}
	// empty status renders empty (task not started yet).
	empty := &CopyTask{}
	if empty.GetStatus() != "" {
		t.Errorf("empty GetStatus() = %q, want empty", empty.GetStatus())
	}
}

// TestUploadTask_KeysRenderPerLocale checks the upload task's status/name keys.
func TestUploadTask_KeysRenderPerLocale(t *testing.T) {
	ut := &UploadTask{}
	if key := ut.GetNameKey(); key != "task.upload" {
		t.Fatalf("GetNameKey() = %q, want task.upload", key)
	}
	if key := ut.GetStatusKey(); key != uploadStatusUploading {
		t.Fatalf("GetStatusKey() = %q, want %q", key, uploadStatusUploading)
	}
	en := i18n.T("en", ut.GetStatusKey())
	zh := i18n.T("zh-CN", ut.GetStatusKey())
	if en != "uploading" || zh == "" || en == zh {
		t.Errorf("upload status did not localize: en=%q zh=%q", en, zh)
	}
	// name key renders with sample params in both locales.
	args := []any{"name", "f.txt", "dst_storage", "/dst", "dst_path", "/d"}
	enName := i18n.T("en", ut.GetNameKey(), args...)
	zhName := i18n.T("zh-CN", ut.GetNameKey(), args...)
	if !strings.Contains(enName, "f.txt") || !strings.Contains(zhName, "f.txt") || enName == zhName {
		t.Errorf("upload name did not localize: en=%q zh=%q", enName, zhName)
	}
}

// TestBatchTaskNotFoundLocalized guards the batch handler's retErrs message,
// which is rendered from the task.not_found key per viewer locale.
func TestBatchTaskNotFoundLocalized(t *testing.T) {
	en := i18n.T("en", "task.not_found")
	zh := i18n.T("zh-CN", "task.not_found")
	if en != "task not found" || zh == "" || en == zh {
		t.Errorf("task.not_found did not localize: en=%q zh=%q", en, zh)
	}
}
