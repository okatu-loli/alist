package aria2

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
)

const (
	// ExtensionID is the manageable built-in extension id this tool registers
	// under. It differs from ToolName: the offline_download_tool setting selects
	// on ToolName, while the enabled-gate is keyed on this extension id.
	ExtensionID = "builtin.offline_download.aria2"
	// ToolName is the stable tool identity the offline_download_tool setting
	// selects on and Config().Name reports.
	ToolName = "aria2"
)

// Tool is the aria2 ext.OfflineDownloadTool. It lazily (re)builds its minimal
// JSON-RPC client from the aria2_uri / aria2_secret settings and gates readiness
// on an aria2.getVersion probe: a down/misconfigured daemon makes AddURL/Status
// return a clear error, which is non-fatal at registration time (offline
// download simply keeps falling back to SimpleHttp until the daemon is up).
type Tool struct {
	mu         sync.Mutex
	client     *Client
	lastURI    string
	lastSecret string
}

// Compile-time proof the tool satisfies the extension contract.
var _ ext.OfflineDownloadTool = (*Tool)(nil)

// --- ext.Extension ---

// Manifest identifies aria2 as an independent built-in of Kind
// OfflineDownloadTool. Its config lives in the shared OFFLINE_DOWNLOAD settings
// group (the /@manage/setting/offline_download page the base surfaces); this
// manifest only names the extension for the registry.
func (*Tool) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ExtensionID,
		Name:  map[string]string{"en": "Offline: aria2", "zh": "离线下载:aria2"},
		Kinds: []ext.Kind{ext.KindOfflineDownloadTool},
	}
}

func (*Tool) Init(ctx context.Context, host ext.Host) error { return nil }
func (*Tool) Shutdown(ctx context.Context) error            { return nil }

// --- ext.OfflineDownloadTool ---

// Config reports the tool identity the offline_download_tool setting selects on.
func (*Tool) Config() ext.OfflineDownloadToolConfig {
	return ext.OfflineDownloadToolConfig{Name: ToolName}
}

// ensureClient returns a ready client, rebuilding it whenever aria2_uri /
// aria2_secret change (or on first use). Building probes aria2.getVersion so a
// down daemon surfaces a clear error here rather than deep in a call.
func (t *Tool) ensureClient() (*Client, error) {
	uri := setting.GetStr(conf.Aria2Uri)
	secret := setting.GetStr(conf.Aria2Secret)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil && t.lastURI == uri && t.lastSecret == secret {
		return t.client, nil
	}
	c := New(uri, secret)
	if _, err := c.GetVersion(); err != nil {
		// Do not cache an unready client; the next call retries.
		t.client = nil
		return nil, fmt.Errorf("aria2 daemon not ready at %s: %w", uri, err)
	}
	t.client = c
	t.lastURI = uri
	t.lastSecret = secret
	return t.client, nil
}

// AddURL hands aria2 the URL to fetch into args.TempDir (the "dir" option, so the
// transfer engine finds the files there) and returns the daemon-side gid.
func (t *Tool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	c, err := t.ensureClient()
	if err != nil {
		return "", err
	}
	return c.AddURI([]string{args.URL}, map[string]string{"dir": args.TempDir})
}

// Status maps aria2.tellStatus onto ext.ToolStatus: TotalBytes/Progress from the
// length strings (div-by-zero guarded to 0), NewGID from FollowedBy[0] (aria2
// promoting a metadata download to the real file/torrent), Completed on
// status=="complete", and Err carrying the daemon-side message on status=="error".
func (t *Tool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	c, err := t.ensureClient()
	if err != nil {
		return ext.ToolStatus{}, err
	}
	info, err := c.TellStatus(gid)
	if err != nil {
		return ext.ToolStatus{}, err
	}
	total, _ := strconv.ParseInt(info.TotalLength, 10, 64)
	completed, _ := strconv.ParseInt(info.CompletedLength, 10, 64)
	var progress float64
	if total > 0 {
		progress = float64(completed) / float64(total) * 100
	}
	st := ext.ToolStatus{
		TotalBytes: total,
		Progress:   progress,
		Status:     info.Status,
		Completed:  info.Status == "complete",
	}
	if len(info.FollowedBy) > 0 {
		st.NewGID = info.FollowedBy[0]
	}
	if info.Status == "error" {
		st.Err = info.ErrorMessage
	}
	return st, nil
}

// Remove stops the download and clears its record, both of which are
// NON-file-deleting on aria2's side — the downloaded temp files MUST survive
// because the async transfer still needs them. It calls aria2.remove (stops the
// download) then best-effort aria2.removeDownloadResult (clears the completed/
// error record). It deliberately never calls any purge/force-remove/deleteFiles
// variant. An error is returned only when BOTH calls fail (a down daemon); if
// either succeeds the removal is considered done.
func (t *Tool) Remove(ctx context.Context, gid string) error {
	c, err := t.ensureClient()
	if err != nil {
		return err
	}
	removeErr := c.Remove(gid)
	resultErr := c.RemoveDownloadResult(gid)
	if removeErr != nil && resultErr != nil {
		return removeErr
	}
	return nil
}
