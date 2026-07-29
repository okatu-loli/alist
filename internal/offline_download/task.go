package offline_download

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/xhofe/tache"
)

// Offline-download task i18n keys; the task list re-renders these per viewer
// locale (see GetNameKey/GetStatusKey).
const (
	offlineDownloadName              = "task.offline_download"
	offlineDownloadStatusDownloading = "task.offline_download_status.downloading"
)

// defaultMaxOfflineDownloadBytes is the fallback per-download size backstop used
// when the offline_download_max_size_gb setting is unset or non-positive. It
// caps how much a single SimpleHttp download may pull from the source before
// op.Put is forced to stop, keeping one task (fed a bogus/adversarial "infinite"
// stream, or a source that just never ends) from occupying a download worker
// forever and filling the destination disk. The bound is deliberately generous
// -- larger than any legitimate single file we expect -- so it never truncates a
// real download; it's a backstop, not a quota. The per-dial timeout in
// simplehttp.go handles the *slow* case; this handles the *endless* case.
const defaultMaxOfflineDownloadBytes int64 = 100 << 30 // 100 GiB

// maxOfflineDownloadBytes resolves the effective per-download size backstop from
// the offline_download_max_size_gb setting (in GiB), read fresh at task-run time
// so an admin change takes effect for new tasks. A non-positive/blank value
// falls back to defaultMaxOfflineDownloadBytes (100 GiB).
func maxOfflineDownloadBytes() int64 {
	gib := setting.GetInt(conf.OfflineDownloadMaxSizeGB, 0)
	if gib <= 0 {
		return defaultMaxOfflineDownloadBytes
	}
	return int64(gib) << 30
}

// errDownloadTooLarge is returned when a SimpleHttp source produces more than
// the size backstop allows. It turns "exceeded the cap" into a hard task
// failure (surfaced to the user, retryable) rather than a silently truncated,
// corrupt file that looks like a success.
var errDownloadTooLarge = errors.New("offline_download: source exceeds the maximum allowed size")

// boundedReader wraps r so that reading strictly more than limit bytes fails
// with errDownloadTooLarge instead of silently stopping at the cap. A plain
// io.LimitReader(r, limit) returns EOF at the boundary, which would let an
// oversized (or bogus/endless) source land a truncated file reported as a
// successful download -- a data-integrity hole. Here the underlying reader is
// allowed one extra byte (limit+1); observing that byte proves the source is
// over the cap and turns the download into an error rather than a truncation.
type boundedReader struct {
	r io.Reader // io.LimitReader(src, limit+1)
	n int64     // bytes delivered so far
	// limit is the maximum number of bytes a legitimate download may deliver.
	limit int64
}

func newBoundedReader(r io.Reader, limit int64) *boundedReader {
	return &boundedReader{r: io.LimitReader(r, limit+1), limit: limit}
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		// The source produced the (limit+1)th byte: it is over the cap. Report
		// the overflow rather than the EOF io.LimitReader would otherwise fake.
		return n, errDownloadTooLarge
	}
	return n, err
}

// DownloadTask offline-downloads a single HTTP(S) URL into a destination
// storage directory. Every field is JSON-serializable, so a task survives a
// persist/recover cycle untouched: Run() re-resolves the live storage handle
// (op.GetStorageByMountPath) and re-fetches the URL from scratch. Unlike
// UploadTask there is no on-disk temp file to relocate/reopen -- an offline
// download is inherently re-runnable -- so there is no rebuildFromPersisted
// equivalent here.
type DownloadTask struct {
	task.TaskExtension
	URL              string `json:"url"`
	DstStorageMp     string `json:"dst_storage_mp"`
	DstDirActualPath string `json:"dst_dir_actual_path"`
	// fileNameMu guards FileName: runSimpleHttp (the tache worker goroutine)
	// writes it once Run() resolves the download's real name, while
	// tache.Manager.persist() (on by default -- see
	// conf.Conf.Tasks.Download.TaskPersistant) reflects over every exported
	// field, including this one, from its own debounced goroutine -- a data
	// race under -race, independent of the TaskExtension method-shadow lock
	// (reflection bypasses methods entirely). See MarshalJSON below, which
	// substitutes a locked snapshot for tache's default per-field reflection.
	fileNameMu sync.RWMutex
	// FileName may be empty at creation time; Run() derives it from the
	// response (Content-Disposition / URL path) on first execution and writes
	// it back here so a later listing/retry shows the resolved name.
	FileName string `json:"file_name"`

	// Toolname selects the execution path. Empty or "SimpleHttp" keeps the
	// built-in direct op.Put/PutURL path (unchanged). Any other value routes the
	// task through the active daemon tool (ActiveTool): AddURL into TempDir →
	// poll Status → on completion transfer the temp files into the destination
	// storage → Remove the daemon task.
	Toolname string `json:"toolname"`
	// TempDir is the local scratch directory a daemon tool downloads into
	// (conf.TempDir/<tool>/<uuid>); transferStd later moves its contents into the
	// destination storage. Unused by the SimpleHttp direct path.
	TempDir string `json:"temp_dir"`
	// GID is the daemon-side download id returned by AddURL (and updated as the
	// daemon re-keys it via ToolStatus.NewGID). Transient: it is meaningless
	// across a daemon restart, so it is not persisted.
	GID string `json:"-"`
}

// GetName returns the English rendering for logging/back-compat; the task list
// re-renders GetNameKey/GetNameArgs per viewer locale.
func (t *DownloadTask) GetName() string {
	return i18n.T(i18n.DefaultLang, t.GetNameKey(), t.GetNameArgs()...)
}

func (t *DownloadTask) GetNameKey() string {
	return offlineDownloadName
}

func (t *DownloadTask) GetNameArgs() []any {
	return []any{
		"url", t.URL,
		"dst_storage", t.DstStorageMp,
		"dst_path", t.DstDirActualPath,
	}
}

func (t *DownloadTask) GetStatus() string {
	return i18n.T(i18n.DefaultLang, offlineDownloadStatusDownloading)
}

func (t *DownloadTask) GetStatusKey() string {
	return offlineDownloadStatusDownloading
}

func (t *DownloadTask) GetStatusArgs() []any {
	return nil
}

// setFileName writes FileName under fileNameMu; see fileNameMu's doc comment.
func (t *DownloadTask) setFileName(name string) {
	t.fileNameMu.Lock()
	defer t.fileNameMu.Unlock()
	t.FileName = name
}

// getFileName reads FileName under fileNameMu; see fileNameMu's doc comment.
func (t *DownloadTask) getFileName() string {
	t.fileNameMu.RLock()
	defer t.fileNameMu.RUnlock()
	return t.FileName
}

// MarshalJSON overrides tache's default reflection-based marshal for
// tache.Base's State/Retry/MaxRetry fields (see MoveTask.MarshalJSON for the
// mechanism) AND this type's own FileName, which -- unlike other task types'
// own fields -- IS mutated after the task becomes visible to persist() (see
// fileNameMu's doc comment on the field above).
func (t *DownloadTask) MarshalJSON() ([]byte, error) {
	type Alias DownloadTask
	state, retry, maxRetry := t.PersistSnapshot()
	fileName := t.getFileName()
	return json.Marshal(&struct {
		State    tache.State `json:"state"`
		Retry    int         `json:"retry"`
		MaxRetry int         `json:"max_retry"`
		FileName string      `json:"file_name"`
		*Alias
	}{
		State:    state,
		Retry:    retry,
		MaxRetry: maxRetry,
		FileName: fileName,
		Alias:    (*Alias)(t),
	})
}

// Run dispatches on Toolname. An empty/"SimpleHttp" selection keeps the built-in
// direct path (runSimpleHttp, unchanged). Any other value routes through the
// active daemon tool (runDaemon). It is safe to re-run after a restart: the
// direct path re-resolves storage by mount path and re-fetches the URL; the
// daemon path re-issues AddURL (the transient GID is not persisted).
func (t *DownloadTask) Run() error {
	t.ReinitCtx()
	t.ClearEndTime()
	t.SetStartTime(time.Now())
	defer func() { t.SetEndTime(time.Now()) }()

	if t.Toolname == "" || t.Toolname == "SimpleHttp" {
		return t.runSimpleHttp()
	}
	return t.runDaemon()
}

// runSimpleHttp resolves the destination storage, prefers a driver-native cloud
// pull (op.PutURL), and only falls back to the SSRF-safe server-side SimpleHttp
// stream when the driver does not implement PutURL. This is the v4 base's
// always-available direct path; its semantics are unchanged by the daemon-tool
// work — the daemon path (runDaemon) also falls back here when no tool is active.
func (t *DownloadTask) runSimpleHttp() error {
	storage, err := op.GetStorageByMountPath(t.DstStorageMp)
	if err != nil {
		return err
	}

	// Prefer letting the driver pull the URL itself (index-based drivers, or
	// backends that ingest a URL server-side). PutURL needs a destination name
	// up front, so when the caller left FileName empty we derive a provisional
	// one from the URL (no network access). Only errs.NotImplement means "this
	// driver can't do PutURL" -- any other error is a real failure of an
	// attempted cloud pull and must surface, not be masked by silently
	// re-downloading through our own process.
	provided := t.FileName
	putName := provided
	if putName == "" {
		putName = fileNameFromURL(t.URL)
	}
	err = op.PutURL(t.Ctx(), storage, t.DstDirActualPath, putName, t.URL)
	if err == nil {
		t.setFileName(putName)
		return nil
	}
	if !errs.IsNotImplement(err) {
		return err
	}

	// SimpleHttp fallback: SSRF-safe server-side streaming download, then put
	// the live stream into the destination storage.
	rc, size, name, err := Fetch(t.Ctx(), t.URL)
	if err != nil {
		return err
	}
	defer rc.Close()

	// Honor a caller-provided name; otherwise use the authoritative name Fetch
	// derived from the response (Content-Disposition / URL), which is preferred
	// over the provisional URL-only name used for the PutURL attempt above.
	fname := provided
	if fname == "" {
		fname = name
	}
	t.setFileName(fname)

	// Bound the stream: neither a bogus Content-Length nor an endless body can
	// pull more than the effective cap into the destination. The cap is read from
	// the offline_download_max_size_gb setting at run time. Unlike a plain
	// io.LimitReader, exceeding the cap is a hard error, not a silent truncation
	// that would leave a corrupt file looking like a successful download.
	limited := newBoundedReader(rc, maxOfflineDownloadBytes())

	// size is -1 when the source didn't report Content-Length. In that case
	// leave Obj.Size at 0 (op.Put/local Put copy to EOF regardless of the
	// declared size; the size only drives the progress ratio) and don't feed a
	// negative total to the task's progress accounting.
	objSize := size
	if objSize < 0 {
		objSize = 0
	} else {
		t.SetTotalBytes(size)
	}

	fs := &stream.FileStream{
		Obj: &model.Object{
			Name:     fname,
			Size:     objSize,
			Modified: time.Now(),
		},
		Reader: limited,
	}
	return op.Put(t.Ctx(), storage, t.DstDirActualPath, fs, t.SetProgress, true)
}

// daemonStatusMaxRetries bounds transient Status() failures before the task
// gives up: a daemon hiccup (a dropped RPC, a momentary connection reset) should
// not fail an in-flight download, but a daemon that keeps erroring must not poll
// forever. Mirrors v3's retry budget (callStatusRetried > 5).
const daemonStatusMaxRetries = 5

// daemonPollInterval is the fallback poll cadence when a tool reports no push
// signal. v3 used a 3s ticker alongside a per-tool notify channel; this wave
// polls on the ticker alone (a tool's push-signal seam is a later concern). It
// is a var (not a const) only so tests can shrink it; production never changes it.
var daemonPollInterval = 3 * time.Second

// runDaemon drives an external download daemon (the active ext.OfflineDownloadTool):
// hand it the URL to fetch into t.TempDir, poll its Status until the download
// completes, then transfer the finished temp files into the destination storage
// and remove the daemon-side task.
//
// The task resolves ITS OWN tool by t.Toolname (ToolByName), NOT the live global
// offline_download_tool setting (ActiveTool): a task created for "aria2" always
// runs on aria2 even if the setting later switches to another tool.
//
// Fallback: if that tool is not resolvable at run time (ToolByName==nil — the
// selected tool was disabled/unregistered since the task was created), runDaemon
// falls back to the SimpleHttp direct path rather than failing. This matches the
// base's resolution ethos (a nil tool always means "use SimpleHttp") and search's
// BFS fallback, and keeps a queued task runnable even after an admin flips the
// tool off. Progress is reported on tache's 0..100 scale (ext.ToolStatus.Progress
// is already a percentage), matching op.Put/SetProgress used everywhere else.
func (t *DownloadTask) runDaemon() error {
	tool := ToolByName(t.Toolname)
	if tool == nil {
		// The task's own tool is no longer resolvable: fall back to the
		// always-available direct path instead of failing the queued task.
		log.Debugf("offline_download: tool %q inactive at run time, falling back to SimpleHttp", t.Toolname)
		return t.runSimpleHttp()
	}

	ctx := t.Ctx()
	gid, err := tool.AddURL(ctx, ext.AddArgs{
		URL:     t.URL,
		TempDir: t.TempDir,
		UID:     t.GetID(),
	})
	if err != nil {
		return err
	}
	t.GID = gid

	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()

	retries := 0
	for {
		select {
		case <-t.CtxDone():
			// Cancelled: best-effort remove the daemon task, then surface the
			// cancellation.
			_ = tool.Remove(ctx, t.GID)
			return ctx.Err()
		case <-ticker.C:
			st, err := tool.Status(ctx, t.GID)
			if err != nil {
				retries++
				log.Errorf("offline_download: failed to get status of %s (gid %s), retried %d times: %s", t.GetID(), t.GID, retries, err)
				if retries > daemonStatusMaxRetries {
					return fmt.Errorf("offline_download: giving up on %s after %d failed status polls: %w", t.GetID(), retries, err)
				}
				continue
			}
			retries = 0

			// A daemon may re-key a download (e.g. aria2 promoting a metadata
			// download to the real file): follow the new gid and keep polling.
			if st.NewGID != "" {
				t.GID = st.NewGID
			}
			t.SetTotalBytes(st.TotalBytes)
			t.SetProgress(st.Progress)

			if st.Err != "" {
				return fmt.Errorf("offline_download: daemon reported failure for %s: %s", t.GetID(), st.Err)
			}
			if st.Completed {
				// Move the finished temp files into the destination storage, then
				// remove the daemon-side task. Seedtime/seeding is a per-tool
				// concern handled in a later wave; here we remove right after
				// enqueuing the transfer.
				if err := transferStd(ctx, t.TempDir, t.DstStorageMp, t.DstDirActualPath, DeleteOnUploadSucceed); err != nil {
					return err
				}
				if err := tool.Remove(ctx, t.GID); err != nil {
					log.Errorf("offline_download: failed to remove daemon task %s (gid %s): %s", t.GetID(), t.GID, err)
				}
				return nil
			}
		}
	}
}

// NewDaemonDownloadTask builds a DownloadTask wired for the daemon path: it
// allocates the per-download temp directory (conf.TempDir/<tool>/<uuid>) the
// active tool downloads into and the transfer engine later drains. The handler
// (AddOfflineDownload) constructs daemon tasks through this so it and tests build
// them the same way, centralizing the TempDir layout.
func NewDaemonDownloadTask(toolname, url, dstStorageMp, dstDirActualPath string) *DownloadTask {
	tempDir := filepath.Join(conf.Conf.TempDir, toolname, uuid.NewString())
	return &DownloadTask{
		URL:              url,
		DstStorageMp:     dstStorageMp,
		DstDirActualPath: dstDirActualPath,
		Toolname:         toolname,
		TempDir:          tempDir,
	}
}

// OfflineDownloadTaskManager runs DownloadTasks. It's constructed by
// bootstrap.InitOfflineDownloadManager (paused, started once storages finish
// loading -- same lifecycle as the upload/copy/move managers).
var OfflineDownloadTaskManager *tache.Manager[*DownloadTask]
