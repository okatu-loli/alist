package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/AlistGo/alist-agent/proto"
)

// Rapid-upload-first executor tests (design SP-H / H1): RunCopyTaskWith must,
// right after reporting running, try cc.TryRapid; on a hit it reports done with
// detail "rapid-upload" and returns WITHOUT touching SrcLink/download/PutContent
// at all. A rapid miss OR a TryRapid transport error falls through to the
// unchanged download+stream path — a TryRapid error is NEVER fatal.

// recordingCapClient records which cap ops RunCopyTaskWith drove, so the tests
// can prove the rapid short-circuit skips the whole streaming path (and that the
// fall-back still streams).
type recordingCapClient struct {
	rapid    bool
	rapidErr error

	srcLink SrcLink

	srcLinkCalled bool
	putCalled     bool
	statuses      []string
	details       []string

	// *Token fields capture the capToken RunCopyTaskWith actually passed to
	// each call, so the cap-renewal integration test (task_renew_test.go) can
	// prove a proactively renewed token reaches the LATER calls in the
	// sequence.
	srcLinkToken string
	putToken     string
	statusTokens []string

	renewCapCalls int
	renewToken    string
	renewExpires  int64
	renewErr      error
}

func (c *recordingCapClient) SrcLink(ctx context.Context, taskID, capToken string) (SrcLink, error) {
	c.srcLinkCalled = true
	c.srcLinkToken = capToken
	return c.srcLink, nil
}

func (c *recordingCapClient) TryRapid(ctx context.Context, taskID, capToken string) (bool, error) {
	return c.rapid, c.rapidErr
}

func (c *recordingCapClient) PutContent(ctx context.Context, taskID, capToken string, body io.Reader, size int64, onPct func(pct int)) error {
	c.putCalled = true
	c.putToken = capToken
	_, _ = io.Copy(io.Discard, body)
	return nil
}

func (c *recordingCapClient) Progress(ctx context.Context, taskID, capToken string, pct int, detail string) error {
	return nil
}

func (c *recordingCapClient) Status(ctx context.Context, taskID, capToken, status, detail string) error {
	c.statuses = append(c.statuses, status)
	c.details = append(c.details, detail)
	c.statusTokens = append(c.statusTokens, capToken)
	return nil
}

func (c *recordingCapClient) RenewCap(ctx context.Context, taskID, capToken string) (string, int64, error) {
	c.renewCapCalls++
	if c.renewErr != nil {
		return "", 0, c.renewErr
	}
	return c.renewToken, c.renewExpires, nil
}

func srcServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunCopyTaskWithRapidHitSkipsStreaming: a rapid hit reports done with the
// "rapid-upload" detail and NEVER calls SrcLink or PutContent.
func TestRunCopyTaskWithRapidHitSkipsStreaming(t *testing.T) {
	cc := &recordingCapClient{rapid: true}
	err := RunCopyTaskWith(t.Context(), cc, Task{TaskID: "t-rapid", Type: "copy", SrcPath: "/a/f.bin", DstDir: "/b"}, StaticToken("tok"))
	if err != nil {
		t.Fatalf("RunCopyTaskWith (rapid hit): %v", err)
	}
	if cc.srcLinkCalled {
		t.Fatal("a rapid hit must NOT fetch the src link")
	}
	if cc.putCalled {
		t.Fatal("a rapid hit must NOT stream content")
	}
	if len(cc.statuses) != 2 || cc.statuses[0] != proto.TaskStatusRunning || cc.statuses[1] != proto.TaskStatusDone {
		t.Fatalf("status sequence = %v, want [running done]", cc.statuses)
	}
	if cc.details[1] != "rapid-upload" {
		t.Fatalf("done detail = %q, want rapid-upload", cc.details[1])
	}
}

// TestRunCopyTaskWithRapidMissFallsBackToStreaming: rapid=false streams the copy
// the normal way (SrcLink -> download -> PutContent -> done).
func TestRunCopyTaskWithRapidMissFallsBackToStreaming(t *testing.T) {
	body := "streamed fallback bytes"
	srv := srcServer(t, body)
	cc := &recordingCapClient{rapid: false, srcLink: SrcLink{URL: srv.URL, Size: int64(len(body))}}

	err := RunCopyTaskWith(t.Context(), cc, Task{TaskID: "t-miss", Type: "copy", SrcPath: "/a/f.bin", DstDir: "/b"}, StaticToken("tok"))
	if err != nil {
		t.Fatalf("RunCopyTaskWith (rapid miss): %v", err)
	}
	if !cc.srcLinkCalled || !cc.putCalled {
		t.Fatalf("a rapid miss must fall back to streaming: srcLink=%v put=%v", cc.srcLinkCalled, cc.putCalled)
	}
	if len(cc.statuses) != 2 || cc.statuses[1] != proto.TaskStatusDone {
		t.Fatalf("status sequence = %v, want [running done]", cc.statuses)
	}
	if cc.details[1] == "rapid-upload" {
		t.Fatal("a streamed copy must not be labelled rapid-upload")
	}
}

// TestRunCopyTaskWithRapidErrorFallsBackToStreaming: a TryRapid transport error
// is NON-fatal — the copy falls back to streaming and still completes.
func TestRunCopyTaskWithRapidErrorFallsBackToStreaming(t *testing.T) {
	body := "streamed after rapid error"
	srv := srcServer(t, body)
	cc := &recordingCapClient{rapidErr: errors.New("channel boom"), srcLink: SrcLink{URL: srv.URL, Size: int64(len(body))}}

	err := RunCopyTaskWith(t.Context(), cc, Task{TaskID: "t-err", Type: "copy", SrcPath: "/a/f.bin", DstDir: "/b"}, StaticToken("tok"))
	if err != nil {
		t.Fatalf("a TryRapid error must be non-fatal, got: %v", err)
	}
	if !cc.srcLinkCalled || !cc.putCalled {
		t.Fatalf("a TryRapid error must fall back to streaming: srcLink=%v put=%v", cc.srcLinkCalled, cc.putCalled)
	}
	if len(cc.statuses) != 2 || cc.statuses[1] != proto.TaskStatusDone {
		t.Fatalf("status sequence = %v, want [running done]", cc.statuses)
	}
}
