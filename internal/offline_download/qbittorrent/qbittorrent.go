package qbittorrent

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
	ExtensionID = "builtin.offline_download.qbittorrent"
	// ToolName is the stable tool identity the offline_download_tool setting
	// selects on and Config().Name reports.
	ToolName = "qBittorrent"
)

// completedStates is the set of qBittorrent torrent states that mean the DOWNLOAD
// has finished (the torrent has moved on to seeding / is paused-after-complete /
// is checking a completed download). Progress>=1.0 is the primary completion
// signal; this set is a secondary/robust signal for daemons that report an UP
// state before Progress ticks to exactly 1.
var completedStates = map[string]bool{
	"uploading":  true,
	"stalledUP":  true,
	"forcedUP":   true,
	"queuedUP":   true,
	"pausedUP":   true,
	"checkingUP": true,
}

// errorStates is the set of qBittorrent states that mean the download failed.
var errorStates = map[string]bool{
	"error":        true,
	"missingFiles": true,
}

// Tool is the qBittorrent ext.OfflineDownloadTool. It lazily (re)builds its
// minimal WebUI client from the qbittorrent_url setting and gates readiness on an
// app/version probe: a down/misconfigured WebUI makes AddURL/Status return a clear
// error, which is non-fatal at registration time (offline download simply keeps
// falling back to SimpleHttp until the WebUI is up).
//
// Identity model (mirrors v3): the host-side UID is used as BOTH the gid returned
// from AddURL AND the qBittorrent category the torrent is added under. qBittorrent
// returns no id from add, so the category is the tie-back — Status/Remove look the
// torrent up by category==gid.
type Tool struct {
	mu      sync.Mutex
	client  *Client
	lastURL string
}

// Compile-time proof the tool satisfies the extension contract.
var _ ext.OfflineDownloadTool = (*Tool)(nil)

// --- ext.Extension ---

// Manifest identifies qBittorrent as an independent built-in of Kind
// OfflineDownloadTool. Its config lives in the shared OFFLINE_DOWNLOAD settings
// group (the /@manage/setting/offline_download page the base surfaces); this
// manifest only names the extension for the registry.
func (*Tool) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ExtensionID,
		Name:  map[string]string{"en": "Offline: qBittorrent", "zh": "离线下载:qBittorrent"},
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

// ensureClient returns a ready client, rebuilding it whenever qbittorrent_url
// changes (or on first use). Building probes app/version so a down WebUI surfaces
// a clear error here rather than deep in a call.
func (t *Tool) ensureClient() (*Client, error) {
	rawURL := setting.GetStr(conf.QbittorrentUrl)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil && t.lastURL == rawURL {
		return t.client, nil
	}
	c, err := New(rawURL)
	if err != nil {
		t.client = nil
		return nil, fmt.Errorf("qbittorrent url invalid %q: %w", rawURL, err)
	}
	if _, err := c.Version(); err != nil {
		// Do not cache an unready client; the next call retries.
		t.client = nil
		return nil, fmt.Errorf("qbittorrent webui not ready at %s: %w", rawURL, err)
	}
	t.client = c
	t.lastURL = rawURL
	return t.client, nil
}

// AddURL hands qBittorrent the URL to fetch into args.TempDir, tagging the torrent
// with the category args.UID so Status/Remove can find it, and returns args.UID as
// the gid (qBittorrent's add returns no id; the category is the identity). When
// qbittorrent_seedtime > 0 it best-effort sets the torrent's seeding time limit
// after add — see the note on Remove about seedtime under the transfer-then-remove
// lifecycle.
func (t *Tool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	c, err := t.ensureClient()
	if err != nil {
		return "", err
	}
	if err := c.AddNewLink(args.URL, args.TempDir, args.UID); err != nil {
		return "", err
	}
	// Best-effort seeding time limit at add time. This is the only point in the
	// transfer-then-remove lifecycle where a seed limit can be honored without the
	// tool blocking on a long sleep in Remove; see the Remove doc.
	if minutes := seedtimeMinutes(); minutes > 0 {
		// The torrent may not be enumerable by category yet (still fetching
		// metadata), so resolving the hash can fail; that is non-fatal — the limit
		// is a best-effort convenience, not a correctness requirement.
		if infos, err := c.GetInfo(args.UID); err == nil && len(infos) > 0 {
			_ = c.SetShareLimits(infos[0].Hash, minutes)
		}
	}
	return args.UID, nil
}

// Status maps qBittorrent's torrents/info onto ext.ToolStatus. The torrent is
// looked up by category==gid. An empty result (not enumerable yet: fetching
// metadata / queued) returns a non-completed, non-error status. Otherwise the
// first torrent maps: TotalBytes=Size, Progress=Progress*100, Completed when
// Progress>=1.0 OR State is in the UP/complete set, Status=State, and Err from an
// error State.
func (t *Tool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	c, err := t.ensureClient()
	if err != nil {
		return ext.ToolStatus{}, err
	}
	infos, err := c.GetInfo(gid)
	if err != nil {
		return ext.ToolStatus{}, err
	}
	if len(infos) == 0 {
		// Not visible yet (still fetching metadata / queued). Not completed, not an
		// error — the base keeps polling.
		return ext.ToolStatus{Completed: false, Status: "pending"}, nil
	}
	info := infos[0]
	st := ext.ToolStatus{
		TotalBytes: info.Size,
		Progress:   info.Progress * 100,
		Status:     info.State,
		Completed:  info.Progress >= 1.0 || completedStates[info.State],
	}
	if errorStates[info.State] {
		st.Err = info.State
	}
	return st, nil
}

// Remove resolves the torrent hash via GetInfo(gid) then deletes it with
// deleteFiles=false — the downloaded temp files MUST survive because the async
// transfer still needs them. It NEVER passes deleteFiles=true.
//
// Seedtime note: this port removes the torrent right after the transfer completes
// (the B2 transfer-then-remove lifecycle), so full post-download seeding is NOT
// enforced here — Remove never blocks on a long sleep. A best-effort seeding time
// limit is instead applied at add time (AddURL → SetShareLimits) when
// qbittorrent_seedtime > 0; a value of 0 means no seeding limit, honored as-is.
func (t *Tool) Remove(ctx context.Context, gid string) error {
	c, err := t.ensureClient()
	if err != nil {
		return err
	}
	infos, err := c.GetInfo(gid)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		// Nothing to remove (already gone / never enumerable): not an error.
		return nil
	}
	return c.Delete(infos[0].Hash, false)
}

// seedtimeMinutes reads qbittorrent_seedtime (minutes) as a non-negative int; a
// blank/invalid/negative value is treated as 0 (no seeding limit).
func seedtimeMinutes() int {
	n, err := strconv.Atoi(setting.GetStr(conf.QbittorrentSeedtime))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
