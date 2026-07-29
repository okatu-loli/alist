// Package transmission is the third concrete OfflineDownloadTool backend (子项目 B,
// wave B5): builtin.offline_download.transmission, an external BitTorrent daemon
// AList hands a magnet/.torrent to and then polls over the Transmission RPC. Like
// builtin.offline_download.aria2 and builtin.offline_download.qbittorrent it is
// registered as an independent, manageable built-in extension
// (Kinds:[OfflineDownloadTool]) whose instance the offline_download base resolves
// by its Config().Name ("Transmission") when the extension is enabled; disabling
// the extension makes the base fall back to the direct-download path (SimpleHttp).
//
// Ported from v3 internal/offline_download/transmission/client.go, adapted to the
// v4 ext.OfflineDownloadTool contract AND to an injectable client seam: the Tool
// talks to Transmission only through the small rpcClient interface (the three ops
// it uses — TorrentAdd / TorrentGetHashes / TorrentRemove), whose real
// implementation is *transmissionrpc.Client itself. Tests inject a fake rpcClient
// through the dial seam, so the whole backend is unit-testable with no live
// Transmission daemon (mirrors search/meilisearch's meiliClient pattern).
package transmission

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/hekmon/transmissionrpc/v3"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
)

const (
	// ExtensionID is the manageable built-in extension id this tool registers
	// under. It differs from ToolName: the offline_download_tool setting selects
	// on ToolName, while the enabled-gate is keyed on this extension id.
	ExtensionID = "builtin.offline_download.transmission"
	// ToolName is the stable tool identity the offline_download_tool setting
	// selects on and Config().Name reports.
	ToolName = "Transmission"
)

// statusFields are the torrent-get fields Status/Remove read. Requesting only the
// handful we map keeps the RPC cheap.
var statusFields = []string{"id", "hashString", "totalSize", "percentDone", "status", "errorString"}

// rpcClient is the injectable seam over the Transmission RPC: it captures ONLY the
// three operations the Tool performs, so a fake makes the backend unit-testable
// with no live daemon. Its signatures match *transmissionrpc.Client exactly, so
// the real client satisfies it directly with no wrapper. Note gid == a torrent's
// HashString throughout, so the getter is the by-hash variant (v3's TorrentGet
// "ids by hash" maps to transmissionrpc's TorrentGetHashes).
type rpcClient interface {
	// TorrentAdd adds a magnet (Filename) or a base64 .torrent (MetaInfo) into the
	// payload's DownloadDir and returns the added/duplicate torrent.
	TorrentAdd(ctx context.Context, payload transmissionrpc.TorrentAddPayload) (transmissionrpc.Torrent, error)
	// TorrentGetHashes returns the requested fields for the torrents identified by
	// hash (empty hashes => all torrents; used as the readiness probe).
	TorrentGetHashes(ctx context.Context, fields []string, hashes []string) ([]transmissionrpc.Torrent, error)
	// TorrentRemove deletes torrents by numeric id; DeleteLocalData MUST stay false
	// so the transfer engine still finds the downloaded files.
	TorrentRemove(ctx context.Context, payload transmissionrpc.TorrentRemovePayload) error
}

// Tool is the Transmission ext.OfflineDownloadTool. It lazily (re)builds its
// rpcClient from the transmission_uri setting and gates readiness on a cheap
// torrent-get probe: a down/misconfigured daemon makes AddURL/Status return a clear
// error, which is non-fatal at registration time (offline download simply keeps
// falling back to SimpleHttp until the daemon is up).
//
// Identity model: gid == the added torrent's HashString (stable across the
// daemon's numeric-id churn). Remove resolves the numeric id from the hash before
// deleting.
type Tool struct {
	mu      sync.Mutex
	client  rpcClient
	lastURI string
	// dial builds+probes a client for uri; nil selects the real Transmission RPC
	// client. Tests set it to return a fake rpcClient, so no daemon is needed.
	dial func(ctx context.Context, uri string) (rpcClient, error)
}

// Compile-time proof the tool satisfies the extension contract.
var _ ext.OfflineDownloadTool = (*Tool)(nil)

// --- ext.Extension ---

// Manifest identifies Transmission as an independent built-in of Kind
// OfflineDownloadTool. Its config lives in the shared OFFLINE_DOWNLOAD settings
// group (the /@manage/setting/offline_download page the base surfaces); this
// manifest only names the extension for the registry.
func (*Tool) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ExtensionID,
		Name:  map[string]string{"en": "Offline: Transmission", "zh": "离线下载:Transmission"},
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

// ensureClient returns a ready client, rebuilding it whenever transmission_uri
// changes (or on first use). Building probes torrent-get so a down daemon surfaces
// a clear error here rather than deep in a call.
func (t *Tool) ensureClient(ctx context.Context) (rpcClient, error) {
	uri := setting.GetStr(conf.TransmissionUri)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil && t.lastURI == uri {
		return t.client, nil
	}
	dial := t.dial
	if dial == nil {
		dial = realDial
	}
	c, err := dial(ctx, uri)
	if err != nil {
		// Do not cache an unready client; the next call retries.
		t.client = nil
		return nil, err
	}
	t.client = c
	t.lastURI = uri
	return c, nil
}

// realDial builds the production rpcClient: parse the endpoint, construct
// *transmissionrpc.Client (which satisfies rpcClient directly), and probe it with a
// cheap torrent-get so a down/misconfigured daemon errors clearly.
func realDial(ctx context.Context, uri string) (rpcClient, error) {
	endpoint, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("transmission uri invalid %q: %w", uri, err)
	}
	c, err := transmissionrpc.New(endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to init transmission client: %w", err)
	}
	if _, err := c.TorrentGetHashes(ctx, []string{"id"}, nil); err != nil {
		return nil, fmt.Errorf("transmission daemon not ready at %s: %w", uri, err)
	}
	return c, nil
}

// AddURL hands Transmission the URL to fetch into args.TempDir (DownloadDir, so the
// transfer engine finds the files there) and returns the added torrent's
// HashString as the gid. Mirroring v3: an http/https URL is fetched, base64-encoded
// and passed as MetaInfo (the .torrent content); any other URL (magnet, etc.) is
// passed as Filename for the daemon to resolve.
//
// Seedtime note: transmission_seedtime is NOT enforced here. The transfer-then-
// remove lifecycle removes the torrent right after transfer, and the minimal
// rpcClient seam exposes no seed-limit mutator, so post-download seeding is not
// applied; Remove never blocks on a sleep.
func (t *Tool) AddURL(ctx context.Context, args ext.AddArgs) (string, error) {
	c, err := t.ensureClient(ctx)
	if err != nil {
		return "", err
	}

	tempDir := args.TempDir
	payload := transmissionrpc.TorrentAddPayload{DownloadDir: &tempDir}

	if endpoint, perr := url.Parse(args.URL); perr == nil && (endpoint.Scheme == "http" || endpoint.Scheme == "https") {
		// http(s) URL → fetch the .torrent bytes and pass them base64-encoded.
		b64, ferr := fetchTorrentBase64(ctx, args.URL)
		if ferr != nil {
			return "", ferr
		}
		payload.MetaInfo = &b64
	} else {
		// magnet / other → let the daemon resolve it by filename/URL.
		u := args.URL
		payload.Filename = &u
	}

	torrent, err := c.TorrentAdd(ctx, payload)
	if err != nil {
		return "", err
	}
	if torrent.HashString == nil {
		return "", errors.New("transmission add returned no hash")
	}
	return *torrent.HashString, nil
}

// Status maps Transmission's torrent-get onto ext.ToolStatus. The torrent is looked
// up by hash==gid. An empty result (not enumerable yet: metadata/queued) returns a
// non-completed, non-error status. Otherwise the first torrent maps: TotalBytes from
// totalSize (cunits bits → bytes), Progress=percentDone*100, Completed when
// percentDone>=1 OR the daemon has moved on to seeding (status 5 seed-wait / 6
// seed), Status=the status string, and Err from a non-empty errorString.
func (t *Tool) Status(ctx context.Context, gid string) (ext.ToolStatus, error) {
	c, err := t.ensureClient(ctx)
	if err != nil {
		return ext.ToolStatus{}, err
	}
	torrents, err := c.TorrentGetHashes(ctx, statusFields, []string{gid})
	if err != nil {
		return ext.ToolStatus{}, err
	}
	if len(torrents) == 0 {
		// Not visible yet (still fetching metadata / queued). Not completed, not an
		// error — the base keeps polling.
		return ext.ToolStatus{Completed: false, Status: "pending"}, nil
	}
	info := torrents[0]

	st := ext.ToolStatus{}
	if info.TotalSize != nil {
		st.TotalBytes = int64(info.TotalSize.Byte())
	}
	if info.PercentDone != nil {
		st.Progress = *info.PercentDone * 100
		if *info.PercentDone >= 1 {
			st.Completed = true
		}
	}
	if info.Status != nil {
		st.Status = info.Status.String()
		if *info.Status == transmissionrpc.TorrentStatusSeedWait || *info.Status == transmissionrpc.TorrentStatusSeed {
			st.Completed = true
		}
	}
	if info.ErrorString != nil && *info.ErrorString != "" {
		st.Err = *info.ErrorString
	}
	return st, nil
}

// Remove resolves the numeric torrent id from the hash (gid) then deletes it with
// DeleteLocalData=false — the downloaded temp files MUST survive because the async
// transfer still needs them. It NEVER passes DeleteLocalData=true, and never blocks
// on a seed-time sleep.
func (t *Tool) Remove(ctx context.Context, gid string) error {
	c, err := t.ensureClient(ctx)
	if err != nil {
		return err
	}
	torrents, err := c.TorrentGetHashes(ctx, []string{"id"}, []string{gid})
	if err != nil {
		return err
	}
	if len(torrents) == 0 || torrents[0].ID == nil {
		// Nothing to remove (already gone / never enumerable): not an error.
		return nil
	}
	return c.TorrentRemove(ctx, transmissionrpc.TorrentRemovePayload{
		IDs:             []int64{*torrents[0].ID},
		DeleteLocalData: false,
	})
}

// fetchTorrentBase64 GETs an http(s) .torrent URL and streams its body into a
// base64 string suitable for TorrentAddPayload.MetaInfo.
func fetchTorrentBase64(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build .torrent request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get .torrent file: %w", err)
	}
	defer resp.Body.Close()

	var buf strings.Builder
	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	if _, err := io.Copy(encoder, resp.Body); err != nil {
		return "", fmt.Errorf("failed to encode .torrent file: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf("failed to flush .torrent encoder: %w", err)
	}
	return buf.String(), nil
}
