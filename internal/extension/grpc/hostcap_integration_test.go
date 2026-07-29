package grpc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/AlistGo/alist-ext/grpcplugin"
	pb "github.com/AlistGo/alist-ext/grpcproto"
	_ "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/extension/grpc/host"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
)

// This is the F0 reverse-bridge integration gate: a real plugin subprocess
// borrows Host FS/Store/HTTP through the plugin->host gRPC bridge, and the host
// asserts the security invariants hold across the wire — FS runs core Authorize,
// Store is namespaced per plugin, HTTP obeys the allowlist ∩ Grant.

var buildHostcapOnce sync.Once
var hostcapBin, hostcapBuildSkip string

func hostcapPluginBin(t *testing.T) string {
	t.Helper()
	buildHostcapOnce.Do(func() {
		dir, err := os.MkdirTemp("", "alist-hostcap-*")
		if err != nil {
			hostcapBuildSkip = err.Error()
			return
		}
		out := filepath.Join(dir, "hostcapplugin")
		cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/hostcapplugin")
		if b, err := cmd.CombinedOutput(); err != nil {
			hostcapBuildSkip = fmt.Sprintf("build hostcapplugin: %v\n%s", err, b)
			return
		}
		hostcapBin = out
	})
	if hostcapBuildSkip != "" {
		t.Skip("skip hostcap integration: " + hostcapBuildSkip)
	}
	return hostcapBin
}

// launchProbe starts the probe plugin subprocess and returns its client (which
// exposes the go-plugin broker) plus a cleanup.
func launchProbe(t *testing.T) *grpcplugin.ProbeClient {
	t.Helper()
	bin := hostcapPluginBin(t)
	pc := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  grpcplugin.Handshake,
		Plugins:          grpcplugin.HostCapProbePluginMap(nil), // host side: Impl nil
		Cmd:              exec.Command(bin),
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           hclog.NewNullLogger(),
	})
	t.Cleanup(pc.Kill)
	rpcClient, err := pc.Client()
	if err != nil {
		t.Fatalf("plugin client: %v", err)
	}
	raw, err := rpcClient.Dispense(pb.HostCapProbePluginName)
	if err != nil {
		t.Fatalf("dispense: %v", err)
	}
	probe, ok := raw.(*grpcplugin.ProbeClient)
	if !ok {
		t.Fatalf("dispensed %T is not *ProbeClient", raw)
	}
	return probe
}

// serveHost stands up a HostService for h on a fresh broker id and returns the
// id the plugin should Dial.
func serveHost(probe *grpcplugin.ProbeClient, h *hostcap.Host) uint32 {
	id := probe.Broker.NextId()
	go probe.Broker.AcceptAndServe(id, host.NewHostServiceServer(h))
	return id
}

func mountHostcapLocal(t *testing.T, mountPath string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := op.CreateStorage(context.Background(), model.Storage{
		MountPath: mountPath,
		Driver:    "Local",
		Addition:  fmt.Sprintf(`{"root_folder_path":%q}`, root),
	}); err != nil {
		t.Fatalf("mount local: %v", err)
	}
}

// TestReverseBridgeFSAuthorize: a plugin lists an allowed subtree over the
// bridge (invariant 1 allow), is denied a path outside its grant (invariant 1
// deny), and can write when granted write.
func TestReverseBridgeFSAuthorize(t *testing.T) {
	mnt := "/hostcap-fs"
	mountHostcapLocal(t, mnt)
	probe := launchProbe(t)
	ctx := context.Background()

	h := hostcap.New(hostcap.Grant{
		PluginID: "it.fs",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapFS}},
		FS:       []hostcap.FSScope{{Path: mnt, Write: true}},
	}, hostcap.Options{})
	id := serveHost(probe, h)

	got, err := probe.Exercise(ctx, id, "fs.list", mnt, "")
	if err != nil {
		t.Fatalf("fs.list allowed: %v", err)
	}
	if got != "seed.txt,sub" {
		t.Fatalf("fs.list = %q, want %q", got, "seed.txt,sub")
	}

	// A path outside the grant is denied at the host's Authorize, over the wire.
	if _, err := probe.Exercise(ctx, id, "fs.list", "/elsewhere", ""); err == nil {
		t.Fatal("fs.list outside grant: expected permission error, got nil")
	}

	// Write is authorized (grant has Write); the new dir appears.
	if _, err := probe.Exercise(ctx, id, "fs.mkdir", mnt, "made-by-plugin"); err != nil {
		t.Fatalf("fs.mkdir: %v", err)
	}
	got, err = probe.Exercise(ctx, id, "fs.list", mnt, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "made-by-plugin,seed.txt,sub" {
		t.Fatalf("after mkdir fs.list = %q", got)
	}
}

// TestReverseBridgeFSDeniedWithoutGrant: a plugin with no fs capability is
// denied even for an otherwise-valid path (invariant 5).
func TestReverseBridgeFSDeniedWithoutGrant(t *testing.T) {
	mnt := "/hostcap-nofs"
	mountHostcapLocal(t, mnt)
	probe := launchProbe(t)
	ctx := context.Background()

	h := hostcap.New(hostcap.Grant{
		PluginID: "it.nofs",
		FS:       []hostcap.FSScope{{Path: mnt}}, // scope present, capability NOT granted
	}, hostcap.Options{})
	id := serveHost(probe, h)

	if _, err := probe.Exercise(ctx, id, "fs.list", mnt, ""); err == nil {
		t.Fatal("fs.list without CapFS: expected permission error, got nil")
	}
}

// TestReverseBridgeStoreIsolation: two plugins share one backend; plugin B
// cannot read plugin A's key (invariant 3), and an ungranted plugin is denied
// (invariant 5) — all across the real bridge.
func TestReverseBridgeStoreIsolation(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()
	backend := hostcap.NewMemKVBackend()

	hostA := hostcap.New(hostcap.Grant{PluginID: "it.storeA", HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}}}, hostcap.Options{KVBackend: backend})
	hostB := hostcap.New(hostcap.Grant{PluginID: "it.storeB", HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}}}, hostcap.Options{KVBackend: backend})
	hostC := hostcap.New(hostcap.Grant{PluginID: "it.storeC"}, hostcap.Options{KVBackend: backend}) // no store cap
	idA := serveHost(probe, hostA)
	idB := serveHost(probe, hostB)
	idC := serveHost(probe, hostC)

	if _, err := probe.Exercise(ctx, idA, "store.set", "k", "A-secret"); err != nil {
		t.Fatalf("A set: %v", err)
	}
	// B reads its own namespace — must not see A's key.
	if got, err := probe.Exercise(ctx, idB, "store.get", "k", ""); err != nil || got != "<missing>" {
		t.Fatalf("B get = %q,%v; want <missing>,nil (isolation)", got, err)
	}
	if got, err := probe.Exercise(ctx, idA, "store.get", "k", ""); err != nil || got != "A-secret" {
		t.Fatalf("A get = %q,%v; want A-secret", got, err)
	}
	// C was not granted store.
	if _, err := probe.Exercise(ctx, idC, "store.set", "k", "v"); err == nil {
		t.Fatal("C set without CapStore: expected permission error, got nil")
	}
}

// TestReverseBridgeTableRoundTripAndIsolation: a plugin inserts/gets/lists
// rows in a named table over the real plugin->host gRPC bridge (Table* RPCs),
// mirroring TestReverseBridgeStoreIsolation's Store().KV() coverage — round
// trip, per-plugin isolation, and the CapStore grant gate all hold across the
// real wire, not just in-process (hostcap's table_test.go already covers the
// in-process leg exhaustively).
func TestReverseBridgeTableRoundTripAndIsolation(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()
	backend := hostcap.NewMemKVBackend()

	hostA := hostcap.New(hostcap.Grant{PluginID: "it.tableA", HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}}}, hostcap.Options{KVBackend: backend})
	hostB := hostcap.New(hostcap.Grant{PluginID: "it.tableB", HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}}}, hostcap.Options{KVBackend: backend})
	hostC := hostcap.New(hostcap.Grant{PluginID: "it.tableC"}, hostcap.Options{KVBackend: backend}) // no store cap
	idA := serveHost(probe, hostA)
	idB := serveHost(probe, hostB)
	idC := serveHost(probe, hostC)

	// A inserts a row and reads it back.
	rowID, err := probe.Exercise(ctx, idA, "table.insert", "widgets", `{"name":"sprocket"}`)
	if err != nil {
		t.Fatalf("table.insert: %v", err)
	}
	if rowID == "" {
		t.Fatal("table.insert returned empty id")
	}
	got, err := probe.Exercise(ctx, idA, "table.get", "widgets", rowID)
	if err != nil {
		t.Fatalf("table.get: %v", err)
	}
	if got != `{"name":"sprocket"}` {
		t.Fatalf("table.get = %q, want the inserted row JSON", got)
	}

	// B (a different plugin sharing the backend) cannot see A's row —
	// per-plugin isolation holds across the bridge.
	if got, err := probe.Exercise(ctx, idB, "table.get", "widgets", rowID); err != nil || got != "<missing>" {
		t.Fatalf("B table.get A's row = %q,%v; want <missing>,nil (isolation)", got, err)
	}
	listB, err := probe.Exercise(ctx, idB, "table.list", "widgets", "")
	if err != nil {
		t.Fatalf("B table.list: %v", err)
	}
	if listB != "" {
		t.Fatalf("B table.list = %q, want empty (per-plugin isolation)", listB)
	}

	// A's own list shows exactly the one row it inserted.
	listA, err := probe.Exercise(ctx, idA, "table.list", "widgets", "")
	if err != nil {
		t.Fatalf("A table.list: %v", err)
	}
	if listA != rowID+`:{"name":"sprocket"}` {
		t.Fatalf("A table.list = %q, want %q", listA, rowID+`:{"name":"sprocket"}`)
	}

	// C was not granted store: every Table op is denied over the bridge.
	if _, err := probe.Exercise(ctx, idC, "table.insert", "widgets", "{}"); err == nil {
		t.Fatal("C table.insert without CapStore: expected permission error, got nil")
	}
}

// TestReverseBridgeHTTPAllowlist: over the bridge, a host on the plugin's
// allowlist is reachable (invariant 2 allow), a host off it is refused
// (invariant 2 deny), and a plugin without the network capability is refused
// (invariant 5).
func TestReverseBridgeHTTPAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	allowedHost := u.Hostname()

	probe := launchProbe(t)
	ctx := context.Background()

	// Granted network + host on allowlist.
	hostOK := hostcap.New(hostcap.Grant{
		PluginID: "it.httpOK",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapNetwork}},
		Network:  []string{allowedHost},
	}, hostcap.Options{OutboundPolicy: hostcap.AllowAllOutbound})
	idOK := serveHost(probe, hostOK)
	if got, err := probe.Exercise(ctx, idOK, "http.get", srv.URL, ""); err != nil || got != "200:ok" {
		t.Fatalf("http.get allowed = %q,%v; want 200:ok", got, err)
	}

	// Granted network but the target host is NOT on the allowlist.
	hostOff := hostcap.New(hostcap.Grant{
		PluginID: "it.httpOff",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapNetwork}},
		Network:  []string{"other.example.com"},
	}, hostcap.Options{OutboundPolicy: hostcap.AllowAllOutbound})
	idOff := serveHost(probe, hostOff)
	if _, err := probe.Exercise(ctx, idOff, "http.get", srv.URL, ""); err == nil {
		t.Fatal("http.get off allowlist: expected permission error, got nil")
	}

	// No network capability at all.
	hostNo := hostcap.New(hostcap.Grant{PluginID: "it.httpNo", Network: []string{allowedHost}}, hostcap.Options{OutboundPolicy: hostcap.AllowAllOutbound})
	idNo := serveHost(probe, hostNo)
	if _, err := probe.Exercise(ctx, idNo, "http.get", srv.URL, ""); err == nil {
		t.Fatal("http.get without CapNetwork: expected permission error, got nil")
	}
}
