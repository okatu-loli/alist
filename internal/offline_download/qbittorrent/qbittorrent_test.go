package qbittorrent

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	dB, err := gorm.Open(sqlite.Open("file:qbit_tool_test?mode=memory&cache=shared"),
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

// setQbitSettings points qbittorrent_url at the mock server so the tool's lazy
// connect builds a client against it. Cleared on cleanup.
func setQbitSettings(t *testing.T, rawURL, seedtime string) {
	t.Helper()
	for _, it := range []model.SettingItem{
		{Key: conf.QbittorrentUrl, Value: rawURL, Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD, Flag: model.PRIVATE},
		{Key: conf.QbittorrentSeedtime, Value: seedtime, Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD},
	} {
		item := it
		if err := op.SaveSettingItem(&item); err != nil {
			t.Fatalf("seed %s: %v", it.Key, err)
		}
	}
	t.Cleanup(func() {
		op.SettingCacheUpdate()
		_ = db.SaveSettingItem(&model.SettingItem{Key: conf.QbittorrentUrl, Value: "", Type: conf.TypeString, Group: model.OFFLINE_DOWNLOAD})
		_ = db.SaveSettingItem(&model.SettingItem{Key: conf.QbittorrentSeedtime, Value: "0", Type: conf.TypeNumber, Group: model.OFFLINE_DOWNLOAD})
		op.SettingCacheUpdate()
	})
}

func TestToolSatisfiesInterface(t *testing.T) {
	var _ ext.OfflineDownloadTool = (*Tool)(nil)
}

func TestToolConfigName(t *testing.T) {
	tool := &Tool{}
	if tool.Config().Name != "qBittorrent" {
		t.Fatalf("Config().Name = %q, want qBittorrent", tool.Config().Name)
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
	if ExtensionID != "builtin.offline_download.qbittorrent" {
		t.Fatalf("ExtensionID = %q, want builtin.offline_download.qbittorrent", ExtensionID)
	}
	if len(m.Kinds) != 1 || m.Kinds[0] != ext.KindOfflineDownloadTool {
		t.Fatalf("manifest kinds = %v", m.Kinds)
	}
}

func TestToolAddURL(t *testing.T) {
	m := newMockQbit(t)
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	gid, err := tool.AddURL(context.Background(), ext.AddArgs{
		URL:     "magnet:?xt=urn:btih:abc",
		TempDir: "/tmp/dl/uidA",
		UID:     "uidA",
	})
	if err != nil {
		t.Fatalf("AddURL: %v", err)
	}
	// gid is the UID (qBittorrent add returns no id; the category ties back).
	if gid != "uidA" {
		t.Fatalf("gid = %q, want uidA", gid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addFields["savepath"] != "/tmp/dl/uidA" {
		t.Fatalf("mock savepath = %q, want /tmp/dl/uidA", m.addFields["savepath"])
	}
	if m.addFields["category"] != "uidA" {
		t.Fatalf("mock category = %q, want uidA", m.addFields["category"])
	}
}

func TestToolStatusCompletedByUPState(t *testing.T) {
	m := newMockQbit(t)
	// A stalledUP torrent with Progress<1: the UP-state set must still mark it done.
	m.infoJSON = `[{"hash":"H","name":"f","state":"stalledUP","progress":0.999,"size":100,"completed":99,"category":"uidA"}]`
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	st, err := tool.Status(context.Background(), "uidA")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Completed {
		t.Fatal("stalledUP must map to Completed")
	}
	if st.TotalBytes != 100 {
		t.Fatalf("TotalBytes = %d, want 100", st.TotalBytes)
	}
	if st.Status != "stalledUP" {
		t.Fatalf("Status = %q, want stalledUP", st.Status)
	}
}

func TestToolStatusCompletedByProgress(t *testing.T) {
	m := newMockQbit(t)
	// Progress==1 while still in a DL-ish state: Progress>=1 is the robust signal.
	m.infoJSON = `[{"hash":"H","name":"f","state":"checkingUP","progress":1.0,"size":100,"completed":100,"category":"uidA"}]`
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	st, err := tool.Status(context.Background(), "uidA")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Completed {
		t.Fatal("progress>=1 must map to Completed")
	}
	if st.Progress != 100 {
		t.Fatalf("Progress = %v, want 100", st.Progress)
	}
}

func TestToolStatusDownloadingNotCompleted(t *testing.T) {
	m := newMockQbit(t)
	m.infoJSON = `[{"hash":"H","name":"f","state":"downloading","progress":0.4,"size":1000,"completed":400,"category":"uidA"}]`
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	st, err := tool.Status(context.Background(), "uidA")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Completed {
		t.Fatal("downloading must not be Completed")
	}
	if st.Progress != 40 {
		t.Fatalf("Progress = %v, want 40", st.Progress)
	}
	if st.Err != "" {
		t.Fatalf("downloading Err = %v, want empty", st.Err)
	}
}

func TestToolStatusErrorState(t *testing.T) {
	m := newMockQbit(t)
	m.infoJSON = `[{"hash":"H","name":"f","state":"missingFiles","progress":0.2,"size":1000,"completed":200,"category":"uidA"}]`
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	st, err := tool.Status(context.Background(), "uidA")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Err != "missingFiles" {
		t.Fatalf("Err = %v, want missingFiles", st.Err)
	}
}

func TestToolStatusEmptyCategoryNotCompleted(t *testing.T) {
	m := newMockQbit(t)
	m.infoJSON = `[]`
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	st, err := tool.Status(context.Background(), "uidA")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Completed {
		t.Fatal("empty category (still queued/metadata) must not be Completed")
	}
	if st.Err != "" {
		t.Fatalf("empty category Err = %v, want empty", st.Err)
	}
}

// TestToolRemoveDeleteFilesFalse proves Remove resolves the hash via GetInfo then
// Delete(hash,false), and that NO deleteFiles=true is ever sent (the temp files
// must survive for the transfer).
func TestToolRemoveDeleteFilesFalse(t *testing.T) {
	m := newMockQbit(t)
	m.infoJSON = `[{"hash":"HASHZ","name":"f","state":"uploading","progress":1.0,"size":100,"completed":100,"category":"uidA"}]`
	setQbitSettings(t, m.urlWithCreds(), "0")

	tool := &Tool{}
	if err := tool.Remove(context.Background(), "uidA"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteForm["hashes"] != "HASHZ" {
		t.Fatalf("delete hashes = %q, want HASHZ (resolved from GetInfo)", m.deleteForm["hashes"])
	}
	if m.deleteForm["deleteFiles"] == "true" {
		t.Fatal("Remove must NEVER send deleteFiles=true")
	}
	if m.deleteForm["deleteFiles"] != "false" {
		t.Fatalf("delete deleteFiles = %q, want false", m.deleteForm["deleteFiles"])
	}
}
