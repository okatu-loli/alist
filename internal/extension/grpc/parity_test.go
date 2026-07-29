package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/db/migration"
	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/alist-org/alist/v3/internal/extension/bridge"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
	"github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urldriver"
	"github.com/alist-org/alist/v3/server/common"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// This is the 9.2 parity gate: run the same operation sequence against (a) the
// local ext driver in-process (the 9.1 bridge) and (b) the same driver served
// through a real go-plugin subprocess over gRPC, and assert identical behavior.
// A synthetic URL-type driver covers the ByteSource==url protocol branch.

var (
	localPluginBin               string
	urlPluginBin                 string
	authPluginBin                string
	authnPluginBin               string
	bgPluginBin                  string
	apiRoutePluginBin            string
	actionPluginBin              string
	eventHookPluginBin           string
	scheduledJobPluginBin        string
	notificationPluginBin        string
	fileDecoratorPluginBin       string
	accessProtocolPluginBin      string
	offlineDownloadToolPluginBin string
	searchIndexerPluginBin       string
	archiveToolPluginBin         string
	buildSkip                    string // non-empty => building fixtures failed; tests t.Skip
)

func TestMain(m *testing.M) {
	// The in-process local bridge's Init queries a setting, so a DB must exist.
	dB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	conf.Conf = conf.DefaultConfig()
	db.Init(dB)
	if err := migration.New(dB, "").Up(); err != nil {
		panic("failed to migrate test database: " + err.Error())
	}
	// The reverse-bridge Auth.VerifyToken round trip (hostcap_integration_test.go)
	// needs real signed/tracked tokens, the same way server/middlewares.Auth does
	// in production (server/router.go, internal/bootstrap/db.go).
	tokenstore.Init(tokenstore.NewSQLiteStore())
	common.SecretKey = []byte("grpc-parity-test-secret")

	tmp, err := os.MkdirTemp("", "alist-parity-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)

	localPluginBin, urlPluginBin, authPluginBin, authnPluginBin, bgPluginBin, apiRoutePluginBin, actionPluginBin,
		eventHookPluginBin, scheduledJobPluginBin, notificationPluginBin, fileDecoratorPluginBin, accessProtocolPluginBin,
		offlineDownloadToolPluginBin, searchIndexerPluginBin, archiveToolPluginBin,
		buildSkip = buildFixtures(tmp)
	os.Exit(m.Run())
}

// buildFixtures compiles the plugin binaries. On any failure it returns a skip
// reason instead of panicking, so environments without a working go toolchain
// skip the gRPC parity (and WS-8 AuthProvider round-trip) tests gracefully.
func buildFixtures(dir string) (localBin, urlBin, authBin, authnBin, bgBin, apiRouteBin, actionBin, eventHookBin, scheduledJobBin, notificationBin, fileDecoratorBin, accessProtocolBin, offlineDownloadToolBin, searchIndexerBin, archiveToolBin, skip string) {
	fail := func(err error) (string, string, string, string, string, string, string, string, string, string, string, string, string, string, string, string) {
		return "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "skip gRPC parity: " + err.Error()
	}
	build := func(name, pkg string) (string, error) {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		if b, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build %s: %v\n%s", pkg, err, b)
		}
		return out, nil
	}
	localBin, err := build("localplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/localplugin")
	if err != nil {
		return fail(err)
	}
	urlBin, err = build("urlplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/urlplugin")
	if err != nil {
		return fail(err)
	}
	authBin, err = build("authplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/authplugin")
	if err != nil {
		return fail(err)
	}
	authnBin, err = build("authnplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/authnplugin")
	if err != nil {
		return fail(err)
	}
	bgBin, err = build("bgplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/bgplugin")
	if err != nil {
		return fail(err)
	}
	apiRouteBin, err = build("apirouteplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/apirouteplugin")
	if err != nil {
		return fail(err)
	}
	actionBin, err = build("actionplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/actionplugin")
	if err != nil {
		return fail(err)
	}
	eventHookBin, err = build("eventhookplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/eventhookplugin")
	if err != nil {
		return fail(err)
	}
	scheduledJobBin, err = build("scheduledjobplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/scheduledjobplugin")
	if err != nil {
		return fail(err)
	}
	notificationBin, err = build("notificationplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/notificationplugin")
	if err != nil {
		return fail(err)
	}
	fileDecoratorBin, err = build("filedecoratorplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/filedecoratorplugin")
	if err != nil {
		return fail(err)
	}
	accessProtocolBin, err = build("accessprotocolplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/accessprotocolplugin")
	if err != nil {
		return fail(err)
	}
	offlineDownloadToolBin, err = build("offlinedownloadtoolplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/offlinedownloadtoolplugin")
	if err != nil {
		return fail(err)
	}
	searchIndexerBin, err = build("searchindexerplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/searchindexerplugin")
	if err != nil {
		return fail(err)
	}
	archiveToolBin, err = build("archivetoolplugin", "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/archivetoolplugin")
	if err != nil {
		return fail(err)
	}
	return localBin, urlBin, authBin, authnBin, bgBin, apiRouteBin, actionBin, eventHookBin, scheduledJobBin, notificationBin, fileDecoratorBin, accessProtocolBin, offlineDownloadToolBin, searchIndexerBin, archiveToolBin, ""
}

const storageID = "1"

func ref(anchor string) ext.ObjRef { return ext.ObjRef{StorageID: storageID, Anchor: anchor} }

// inProcLocal builds an in-process local bridge instance rooted at root.
func inProcLocal(t *testing.T, root string) ext.StorageDriver {
	t.Helper()
	ctor, err := extension.DefaultRegistry.GetStorageDriver("Local")
	if err != nil {
		t.Fatalf("local driver not registered: %v", err)
	}
	e, _ := extension.DefaultRegistry.Get("Local")
	b := bridge.NewStorageBridge(ctor, e.Manifest)
	if err := b.NewInstance(context.Background(), storageID, localConfig(root)); err != nil {
		t.Fatalf("in-proc NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = b.Shutdown(context.Background()) })
	return b
}

// grpcLocal launches the local plugin subprocess rooted at root.
func grpcLocal(t *testing.T, root string) *host.Client {
	t.Helper()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	c, err := host.Launch(context.Background(), exec.Command(localPluginBin), nil)
	if err != nil {
		t.Fatalf("launch local plugin: %v", err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	if err := c.NewInstance(context.Background(), storageID, localConfig(root)); err != nil {
		t.Fatalf("grpc NewInstance: %v", err)
	}
	return c
}

func localConfig(root string) json.RawMessage {
	cfg, _ := json.Marshal(map[string]any{
		"root_folder_path": root,
		"mkdir_perm":       "777",
		"recycle_bin_path": "delete permanently",
	})
	return cfg
}

// normObj drops the volatile/path-dependent fields so results from two roots can
// be compared for parity by their stable shape.
type normObj struct {
	Name  string
	Size  int64
	IsDir bool
}

func norm(objs []ext.Obj) []normObj {
	out := make([]normObj, 0, len(objs))
	for _, o := range objs {
		out = append(out, normObj{Name: o.Name, Size: o.Size, IsDir: o.IsDir})
	}
	return out
}

// TestLocalReadParity drives read ops (List/Get/Link/OpenRange) against the same
// seeded tempdir through both executors and asserts identical results and
// byte-identical content through the stream Link path.
func TestLocalReadParity(t *testing.T) {
	root := t.TempDir()
	content := []byte("parity through gRPC and in-process must be byte-identical")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ip := inProcLocal(t, root)
	gp := grpcLocal(t, root)

	// List root
	ipList, err := ip.List(ctx, ref(root), ext.ListArgs{})
	if err != nil {
		t.Fatalf("in-proc List: %v", err)
	}
	gpList, err := gp.List(ctx, ref(root), ext.ListArgs{})
	if err != nil {
		t.Fatalf("grpc List: %v", err)
	}
	if !sameObjSet(norm(ipList), norm(gpList)) {
		t.Fatalf("List parity mismatch:\n in-proc=%+v\n grpc   =%+v", norm(ipList), norm(gpList))
	}

	// Get seed.txt
	ipGet, err := ip.(ext.Getter).Get(ctx, storageID, "seed.txt")
	if err != nil {
		t.Fatalf("in-proc Get: %v", err)
	}
	gpGet, err := gp.Get(ctx, storageID, "seed.txt")
	if err != nil {
		t.Fatalf("grpc Get: %v", err)
	}
	if ipGet.Name != gpGet.Name || ipGet.Size != gpGet.Size || ipGet.IsDir != gpGet.IsDir {
		t.Fatalf("Get parity mismatch: in-proc=%+v grpc=%+v", ipGet, gpGet)
	}

	// Link + OpenRange: full read, byte-identical
	seed := filepath.Join(root, "seed.txt")
	ipFull := readStreamLink(t, ctx, ip.(ext.RangeOpener), ip, ref(seed), ext.Range{Start: 0, Length: -1})
	gpFull := readStreamLink(t, ctx, gp, gp, ref(seed), ext.Range{Start: 0, Length: -1})
	if string(ipFull) != string(content) {
		t.Fatalf("in-proc stream mismatch: %q", ipFull)
	}
	if string(gpFull) != string(ipFull) {
		t.Fatalf("stream Link parity mismatch:\n in-proc=%q\n grpc   =%q", ipFull, gpFull)
	}

	// Partial range read [10,15)
	ipPart := readStreamLink(t, ctx, ip.(ext.RangeOpener), ip, ref(seed), ext.Range{Start: 10, Length: 15})
	gpPart := readStreamLink(t, ctx, gp, gp, ref(seed), ext.Range{Start: 10, Length: 15})
	if string(ipPart) != string(content[10:25]) {
		t.Fatalf("in-proc partial mismatch: %q want %q", ipPart, content[10:25])
	}
	if string(gpPart) != string(ipPart) {
		t.Fatalf("partial range parity mismatch: in-proc=%q grpc=%q", ipPart, gpPart)
	}
}

// readStreamLink resolves a stream-source Link and reads the requested range
// through OpenRange, returning the bytes.
func readStreamLink(t *testing.T, ctx context.Context, opener ext.RangeOpener, drv ext.StorageDriver, r ext.ObjRef, rng ext.Range) []byte {
	t.Helper()
	link, err := drv.Link(ctx, r, ext.LinkArgs{})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if link.ByteSource != ext.ByteSourceStream || link.StreamID == "" {
		t.Fatalf("expected stream byte source, got %+v", link)
	}
	rc, err := opener.OpenRange(ctx, r, link.StreamID, rng)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return b
}

// TestLocalMutationParity runs the same mutation sequence in two separate roots,
// one through each executor, and asserts the observable results and final trees
// match. It covers MakeDir / Put (pull model) / Rename / Move / Remove.
func TestLocalMutationParity(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	ctx := context.Background()

	ip := inProcLocal(t, rootA)
	gp := grpcLocal(t, rootB)

	resA := runMutationSeq(t, ctx, ip, rootA)
	resB := runMutationSeq(t, ctx, gp, rootB)

	if !equalStrings(resA, resB) {
		t.Fatalf("mutation result parity mismatch:\n in-proc=%v\n grpc   =%v", resA, resB)
	}
	if a, b := treeOf(t, rootA), treeOf(t, rootB); !equalStrings(a, b) {
		t.Fatalf("final tree parity mismatch:\n in-proc=%v\n grpc   =%v", a, b)
	}
}

// runMutationSeq performs a fixed sequence and returns a normalized transcript of
// observable results (independent of the absolute root path).
func runMutationSeq(t *testing.T, ctx context.Context, drv ext.StorageDriver, root string) []string {
	t.Helper()
	var out []string

	// MakeDir sub
	if _, err := drv.(ext.Mkdirer).Mkdir(ctx, ref(root), "sub"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	sub := filepath.Join(root, "sub")

	// Put a.txt into sub via the pull-model upload
	content := []byte("upload via pull model")
	if _, err := drv.(ext.Putter).Put(ctx, ref(sub), &memUpload{name: "a.txt", data: content}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// List sub
	objs, err := drv.List(ctx, ref(sub), ext.ListArgs{})
	if err != nil {
		t.Fatalf("List sub: %v", err)
	}
	for _, o := range norm(objs) {
		out = append(out, fmt.Sprintf("list %s %d dir=%v", o.Name, o.Size, o.IsDir))
	}

	// Rename a.txt -> b.txt
	if _, err := drv.(ext.Renamer).Rename(ctx, ref(filepath.Join(sub, "a.txt")), "b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	out = append(out, "renamed b.txt")

	// Move sub/b.txt -> root
	if _, err := drv.(ext.Mover).Move(ctx, ref(filepath.Join(sub, "b.txt")), ref(root)); err != nil {
		t.Fatalf("Move: %v", err)
	}
	out = append(out, "moved b.txt to root")

	// Remove root/b.txt
	if err := drv.(ext.Remover).Remove(ctx, ref(filepath.Join(root, "b.txt"))); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	out = append(out, "removed b.txt")

	return out
}

// TestURLLinkParity checks the ByteSource==url branch: the synthetic URL driver
// in-process vs through the gRPC subprocess must return the same Link.
func TestURLLinkParity(t *testing.T) {
	ctx := context.Background()
	ip := urldriver.New()

	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	gp, err := host.Launch(ctx, exec.Command(urlPluginBin), nil)
	if err != nil {
		t.Fatalf("launch url plugin: %v", err)
	}
	t.Cleanup(func() { _ = gp.Shutdown(ctx) })
	if err := gp.NewInstance(ctx, storageID, nil); err != nil {
		t.Fatalf("grpc NewInstance: %v", err)
	}

	ipLink, err := ip.Link(ctx, ref(urldriver.FileAnchor), ext.LinkArgs{})
	if err != nil {
		t.Fatalf("in-proc Link: %v", err)
	}
	gpLink, err := gp.Link(ctx, ref(urldriver.FileAnchor), ext.LinkArgs{})
	if err != nil {
		t.Fatalf("grpc Link: %v", err)
	}
	if gpLink.ByteSource != ext.ByteSourceURL {
		t.Fatalf("grpc Link byte source = %q, want url", gpLink.ByteSource)
	}
	if ipLink.URL != gpLink.URL || ipLink.Expiration != gpLink.Expiration {
		t.Fatalf("URL Link parity mismatch: in-proc=%+v grpc=%+v", ipLink, gpLink)
	}
	if ipLink.Header.Get(urldriver.HeaderKey) != gpLink.Header.Get(urldriver.HeaderKey) {
		t.Fatalf("URL Link header parity mismatch: in-proc=%v grpc=%v", ipLink.Header, gpLink.Header)
	}
	if gpLink.Header.Get(urldriver.HeaderKey) != urldriver.HeaderVal {
		t.Fatalf("grpc Link header lost: %v", gpLink.Header)
	}
}

// TestURLCapabilityGate confirms the host proxy gates undeclared capabilities:
// the URL driver declares only Getter, so Remove must return NOT_IMPLEMENT
// without a wire round-trip forcing the plugin to implement it.
func TestURLCapabilityGate(t *testing.T) {
	ctx := context.Background()
	if buildSkip != "" {
		t.Skip(buildSkip)
	}
	gp, err := host.Launch(ctx, exec.Command(urlPluginBin), nil)
	if err != nil {
		t.Fatalf("launch url plugin: %v", err)
	}
	t.Cleanup(func() { _ = gp.Shutdown(ctx) })

	err = gp.Remove(ctx, ref(urldriver.FileAnchor))
	if code, ok := ext.CodeOf(err); !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("Remove on undeclared cap = %v (code %q), want NOT_IMPLEMENT", err, code)
	}
}

// --- helpers ---

func sameObjSet(a, b []normObj) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[normObj]int)
	for _, o := range a {
		seen[o]++
	}
	for _, o := range b {
		seen[o]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// treeOf returns the relative paths of every entry under root, for tree parity.
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		out = append(out, fmt.Sprintf("%s dir=%v size=%d", rel, info.IsDir(), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// memUpload is an in-memory ext.UploadStream (pull model) for tests.
type memUpload struct {
	name string
	data []byte
}

func (m *memUpload) Info() ext.UploadInfo {
	return ext.UploadInfo{Name: m.name, Size: int64(len(m.data))}
}

func (m *memUpload) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if int(off)+n >= len(m.data) {
		return n, io.EOF
	}
	return n, nil
}
