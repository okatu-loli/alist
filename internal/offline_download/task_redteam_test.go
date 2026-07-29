package offline_download

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBoundedReaderErrorsOnOverflow is the load-bearing regression for the
// silent-truncation hardening. A plain io.LimitReader(rc, n) returns EOF at the
// cap, so an oversized/endless source would land a truncated file reported as a
// successful download (a data-integrity hole). newBoundedReader must instead
// fail with errDownloadTooLarge the moment the source exceeds the cap, while a
// source at-or-under the cap streams cleanly to EOF.
//
// Deleting the `if b.n > b.limit { return n, errDownloadTooLarge }` branch (or
// reverting Run to io.LimitReader) turns the over-limit case green-but-truncated
// and this test red.
func TestBoundedReaderErrorsOnOverflow(t *testing.T) {
	t.Run("under-limit streams to EOF", func(t *testing.T) {
		src := strings.NewReader(strings.Repeat("A", 10))
		got, err := io.ReadAll(newBoundedReader(src, 100))
		if err != nil {
			t.Fatalf("under-limit read must succeed, got %v", err)
		}
		if len(got) != 10 {
			t.Fatalf("under-limit delivered %d bytes, want 10", len(got))
		}
	})

	t.Run("exactly-at-limit streams to EOF", func(t *testing.T) {
		src := strings.NewReader(strings.Repeat("A", 100))
		got, err := io.ReadAll(newBoundedReader(src, 100))
		if err != nil {
			t.Fatalf("at-limit read must succeed (exactly limit bytes are legal), got %v", err)
		}
		if len(got) != 100 {
			t.Fatalf("at-limit delivered %d bytes, want 100", len(got))
		}
	})

	t.Run("over-limit errors, not silent truncation", func(t *testing.T) {
		src := strings.NewReader(strings.Repeat("A", 101)) // one byte over
		_, err := io.ReadAll(newBoundedReader(src, 100))
		if !errors.Is(err, errDownloadTooLarge) {
			t.Fatalf("over-limit must fail with errDownloadTooLarge, got %v", err)
		}
	})

	t.Run("endless source errors", func(t *testing.T) {
		// An infinite reader must not hang or silently truncate; it must error.
		_, err := io.ReadAll(newBoundedReader(neverEnding{}, 1<<20))
		if !errors.Is(err, errDownloadTooLarge) {
			t.Fatalf("endless source must fail with errDownloadTooLarge, got %v", err)
		}
	})
}

type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}

// TestDownloadTaskWritesInsideTargetDir_Redteam drives Run() end to end with a
// hostile Content-Disposition (path traversal + absolute) and proves the file
// lands as a bare name INSIDE the destination directory — never at the escaped
// path. The storage root is a fresh temp dir; a sibling "canary" dir outside the
// storage stays empty. This is the FileName-injection -> write-out attack.
func TestDownloadTaskWritesInsideTargetDir_Redteam(t *testing.T) {
	prev := allowLoopbackForTest
	allowLoopbackForTest = true // httptest listens on 127.0.0.1
	t.Cleanup(func() { allowLoopbackForTest = prev })

	body := strings.Repeat("Z", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to escape four levels up and write an absolute-looking name.
		w.Header().Set("Content-Disposition", `attachment; filename="../../../../tmp/pwned"`)
		io.WriteString(w, body)
	}))
	defer srv.Close()

	dstRoot := setupLocalStorage(t, "/od-redteam-name")

	// A canary directory a few levels up from the storage root; a successful
	// traversal would try to write into (or above) it. It must stay empty.
	canary := filepath.Join(dstRoot, "..", "od-canary")
	if err := os.MkdirAll(canary, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(canary) })

	task := &DownloadTask{
		URL:              srv.URL,
		DstStorageMp:     "/od-redteam-name",
		DstDirActualPath: "/",
	}
	if err := runTask(t, task); err != nil {
		t.Fatalf("Run() = %v; want success with a sanitized name", err)
	}

	// The derived name must be the bare final segment, and the bytes must be at
	// <storageRoot>/pwned — inside the storage, not escaped.
	if task.FileName != "pwned" {
		t.Fatalf("sanitized FileName = %q, want %q", task.FileName, "pwned")
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "pwned"))
	if err != nil || string(got) != body {
		t.Fatalf("file must land inside the storage root: content=%q err=%v", got, err)
	}
	// Nothing may have been written outside the storage root.
	entries, _ := os.ReadDir(canary)
	if len(entries) != 0 {
		t.Fatalf("traversal escaped: canary dir %s is not empty: %v", canary, entries)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "..", "tmp", "pwned")); err == nil {
		t.Fatalf("traversal escaped: file written outside storage root")
	}
}
