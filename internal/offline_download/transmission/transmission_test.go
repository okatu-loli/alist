package transmission

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/hekmon/cunits/v2"
	"github.com/hekmon/transmissionrpc/v3"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// init stands up an in-memory settings DB so ensureClient's setting.GetStr lookup
// works. Tests still inject a fake rpcClient via the dial seam, so no live
// Transmission daemon is ever contacted (transmission_uri is left unseeded/empty).
func init() {
	dB, err := gorm.Open(sqlite.Open("file:transmission_tool_test?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		panic(err)
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic(err)
	}
}

// fakeRPC is an injected rpcClient: it records the last payloads and returns
// canned torrents, so the Tool is exercised with no live Transmission daemon.
type fakeRPC struct {
	addPayload    transmissionrpc.TorrentAddPayload
	addResult     transmissionrpc.Torrent
	addErr        error
	getResult     []transmissionrpc.Torrent
	getErr        error
	removePayload transmissionrpc.TorrentRemovePayload
	removeCalled  bool
	removeErr     error
}

func (f *fakeRPC) TorrentAdd(ctx context.Context, payload transmissionrpc.TorrentAddPayload) (transmissionrpc.Torrent, error) {
	f.addPayload = payload
	return f.addResult, f.addErr
}

func (f *fakeRPC) TorrentGetHashes(ctx context.Context, fields []string, hashes []string) ([]transmissionrpc.Torrent, error) {
	return f.getResult, f.getErr
}

func (f *fakeRPC) TorrentRemove(ctx context.Context, payload transmissionrpc.TorrentRemovePayload) error {
	f.removeCalled = true
	f.removePayload = payload
	return f.removeErr
}

// toolWith returns a Tool whose dial seam yields the given fake — ensureClient
// never touches settings/network.
func toolWith(f *fakeRPC) *Tool {
	return &Tool{dial: func(ctx context.Context, uri string) (rpcClient, error) { return f, nil }}
}

func strptr(s string) *string   { return &s }
func i64ptr(i int64) *int64     { return &i }
func f64ptr(f float64) *float64 { return &f }
func bitsFromBytes(b float64) *cunits.Bits {
	v := cunits.ImportInByte(b)
	return &v
}
func statusPtr(s transmissionrpc.TorrentStatus) *transmissionrpc.TorrentStatus { return &s }

func TestToolSatisfiesInterface(t *testing.T) {
	var _ ext.OfflineDownloadTool = (*Tool)(nil)
}

func TestToolConfigName(t *testing.T) {
	tool := &Tool{}
	if tool.Config().Name != "Transmission" {
		t.Fatalf("Config().Name = %q, want Transmission", tool.Config().Name)
	}
	if tool.Config().Name != ToolName {
		t.Fatalf("Config().Name %q != ToolName %q", tool.Config().Name, ToolName)
	}
}

func TestToolManifest(t *testing.T) {
	tool := &Tool{}
	m := tool.Manifest()
	if m.ID != ExtensionID {
		t.Fatalf("manifest ID = %q, want ExtensionID %q", m.ID, ExtensionID)
	}
	if ExtensionID != "builtin.offline_download.transmission" {
		t.Fatalf("ExtensionID = %q, want builtin.offline_download.transmission", ExtensionID)
	}
	if len(m.Kinds) != 1 || m.Kinds[0] != ext.KindOfflineDownloadTool {
		t.Fatalf("manifest kinds = %v", m.Kinds)
	}
}

// TestToolAddURLMagnet: a magnet URL is passed through as Filename with the temp
// dir as DownloadDir, and the returned gid is the added torrent's HashString.
func TestToolAddURLMagnet(t *testing.T) {
	f := &fakeRPC{addResult: transmissionrpc.Torrent{HashString: strptr("HASHMAG"), ID: i64ptr(7)}}
	tool := toolWith(f)

	gid, err := tool.AddURL(context.Background(), ext.AddArgs{
		URL:     "magnet:?xt=urn:btih:abc",
		TempDir: "/tmp/dl/uidA",
		UID:     "uidA",
	})
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if gid != "HASHMAG" {
		t.Fatalf("gid = %q, want HASHMAG (torrent HashString)", gid)
	}
	if f.addPayload.Filename == nil || *f.addPayload.Filename != "magnet:?xt=urn:btih:abc" {
		t.Fatalf("Filename = %v, want the magnet URL", f.addPayload.Filename)
	}
	if f.addPayload.MetaInfo != nil {
		t.Fatalf("MetaInfo must be nil for a magnet, got %v", *f.addPayload.MetaInfo)
	}
	if f.addPayload.DownloadDir == nil || *f.addPayload.DownloadDir != "/tmp/dl/uidA" {
		t.Fatalf("DownloadDir = %v, want /tmp/dl/uidA", f.addPayload.DownloadDir)
	}
}

// TestToolAddURLTorrentFile: an http .torrent URL is fetched (served by httptest),
// base64-encoded and passed as MetaInfo; Filename stays nil.
func TestToolAddURLTorrentFile(t *testing.T) {
	const body = "d8:announce...fake torrent bytes...e"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	f := &fakeRPC{addResult: transmissionrpc.Torrent{HashString: strptr("HASHTOR"), ID: i64ptr(9)}}
	tool := toolWith(f)

	gid, err := tool.AddURL(context.Background(), ext.AddArgs{
		URL:     srv.URL + "/x.torrent",
		TempDir: "/tmp/dl/uidB",
		UID:     "uidB",
	})
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	if gid != "HASHTOR" {
		t.Fatalf("gid = %q, want HASHTOR", gid)
	}
	if f.addPayload.Filename != nil {
		t.Fatalf("Filename must be nil for an http .torrent, got %v", *f.addPayload.Filename)
	}
	if f.addPayload.MetaInfo == nil {
		t.Fatal("MetaInfo must be set for an http .torrent")
	}
	want := base64.StdEncoding.EncodeToString([]byte(body))
	if *f.addPayload.MetaInfo != want {
		t.Fatalf("MetaInfo = %q, want base64(body) %q", *f.addPayload.MetaInfo, want)
	}
	if f.addPayload.DownloadDir == nil || *f.addPayload.DownloadDir != "/tmp/dl/uidB" {
		t.Fatalf("DownloadDir = %v, want /tmp/dl/uidB", f.addPayload.DownloadDir)
	}
}

// TestToolStatusProgress: a downloading torrent maps TotalBytes/Progress and is not
// completed.
func TestToolStatusProgress(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{{
		HashString:  strptr("H"),
		TotalSize:   bitsFromBytes(1000),
		PercentDone: f64ptr(0.4),
		Status:      statusPtr(transmissionrpc.TorrentStatusDownload),
	}}}
	tool := toolWith(f)

	st, err := tool.Status(context.Background(), "H")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Completed {
		t.Fatal("downloading must not be Completed")
	}
	if st.TotalBytes != 1000 {
		t.Fatalf("TotalBytes = %d, want 1000", st.TotalBytes)
	}
	if st.Progress != 40 {
		t.Fatalf("Progress = %v, want 40", st.Progress)
	}
	if st.Status != transmissionrpc.TorrentStatusDownload.String() {
		t.Fatalf("Status = %q, want %q", st.Status, transmissionrpc.TorrentStatusDownload.String())
	}
	if st.Err != "" {
		t.Fatalf("downloading Err = %v, want empty", st.Err)
	}
}

// TestToolStatusCompletedByPercentDone: percentDone>=1 marks Completed.
func TestToolStatusCompletedByPercentDone(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{{
		HashString:  strptr("H"),
		TotalSize:   bitsFromBytes(100),
		PercentDone: f64ptr(1.0),
		Status:      statusPtr(transmissionrpc.TorrentStatusDownload),
	}}}
	tool := toolWith(f)

	st, err := tool.Status(context.Background(), "H")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Completed {
		t.Fatal("percentDone>=1 must map to Completed")
	}
	if st.Progress != 100 {
		t.Fatalf("Progress = %v, want 100", st.Progress)
	}
}

// TestToolStatusCompletedBySeeding: a seeding torrent (status 6) is Completed even
// with percentDone reported below 1.
func TestToolStatusCompletedBySeeding(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{{
		HashString:  strptr("H"),
		TotalSize:   bitsFromBytes(100),
		PercentDone: f64ptr(0.999),
		Status:      statusPtr(transmissionrpc.TorrentStatusSeed),
	}}}
	tool := toolWith(f)

	st, err := tool.Status(context.Background(), "H")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Completed {
		t.Fatal("seeding status must map to Completed")
	}
}

// TestToolStatusErrorString: a non-empty errorString surfaces as Err.
func TestToolStatusErrorString(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{{
		HashString:  strptr("H"),
		TotalSize:   bitsFromBytes(100),
		PercentDone: f64ptr(0.2),
		Status:      statusPtr(transmissionrpc.TorrentStatusStopped),
		ErrorString: strptr("tracker error"),
	}}}
	tool := toolWith(f)

	st, err := tool.Status(context.Background(), "H")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Err != "tracker error" {
		t.Fatalf("Err = %v, want tracker error", st.Err)
	}
}

// TestToolStatusEmptyNotCompleted: an empty result (still fetching metadata) is not
// completed and not an error.
func TestToolStatusEmptyNotCompleted(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{}}
	tool := toolWith(f)

	st, err := tool.Status(context.Background(), "H")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Completed {
		t.Fatal("empty result (metadata/queued) must not be Completed")
	}
	if st.Err != "" {
		t.Fatalf("empty result Err = %v, want empty", st.Err)
	}
}

// TestToolRemoveDeleteLocalDataFalse proves Remove resolves the numeric id from the
// hash then removes with DeleteLocalData=false — NEVER true (temp files must
// survive for the transfer).
func TestToolRemoveDeleteLocalDataFalse(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{{HashString: strptr("H"), ID: i64ptr(42)}}}
	tool := toolWith(f)

	if err := tool.Remove(context.Background(), "H"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !f.removeCalled {
		t.Fatal("TorrentRemove must be called")
	}
	if f.removePayload.DeleteLocalData {
		t.Fatal("Remove must NEVER set DeleteLocalData=true")
	}
	if len(f.removePayload.IDs) != 1 || f.removePayload.IDs[0] != 42 {
		t.Fatalf("remove IDs = %v, want [42] (resolved from hash)", f.removePayload.IDs)
	}
}

// TestToolRemoveMissingNoop: a hash that resolves to nothing is a no-op, not an
// error, and never calls TorrentRemove.
func TestToolRemoveMissingNoop(t *testing.T) {
	f := &fakeRPC{getResult: []transmissionrpc.Torrent{}}
	tool := toolWith(f)

	if err := tool.Remove(context.Background(), "H"); err != nil {
		t.Fatalf("Remove missing: %v", err)
	}
	if f.removeCalled {
		t.Fatal("TorrentRemove must not be called when nothing resolves")
	}
}

// TestToolAddURLPropagatesError: an add error is surfaced.
func TestToolAddURLPropagatesError(t *testing.T) {
	f := &fakeRPC{addErr: errors.New("boom")}
	tool := toolWith(f)
	if _, err := tool.AddURL(context.Background(), ext.AddArgs{URL: "magnet:?x", TempDir: "/tmp/x"}); err == nil {
		t.Fatal("AddURL must propagate the add error")
	}
}
