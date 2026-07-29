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
	"strings"
	"sync"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension/hostcap"
	"github.com/alist-org/alist/v3/internal/extension/pluginproc"
)

// This is the Task 6 e2e gate for the F0 reverse-host bridge (production path):
// a real StorageDriver subprocess is launched through pluginproc.Manager with
// SetHostBuilder installed — the exact wiring internal/bootstrap/plugin_host_
// bridge.go uses in the running server — and the test drives it purely through
// the ordinary DriverService RPCs (no probe protocol). It proves impl.Init was
// actually called with a working ext.Host, that host.HTTP().Do enforces the
// per-plugin Grant.Network allowlist across the wire, and that host.LoadConfig
// delivers the ConfigLoader's value into the subprocess.

var buildHostcallOnce sync.Once
var hostcallBin, hostcallBuildSkip string

func hostcallPluginBin(t *testing.T) string {
	t.Helper()
	buildHostcallOnce.Do(func() {
		dir, err := os.MkdirTemp("", "alist-hostcall-*")
		if err != nil {
			hostcallBuildSkip = err.Error()
			return
		}
		out := filepath.Join(dir, "hostcallplugin")
		cmd := exec.Command("go", "build", "-o", out, "github.com/alist-org/alist/v3/internal/extension/grpc/testdata/hostcallplugin")
		if b, err := cmd.CombinedOutput(); err != nil {
			hostcallBuildSkip = fmt.Sprintf("build hostcallplugin: %v\n%s", err, b)
			return
		}
		hostcallBin = out
	})
	if hostcallBuildSkip != "" {
		t.Skip("skip hostcall integration: " + hostcallBuildSkip)
	}
	return hostcallBin
}

// TestReverseBridgeProductionPath launches the hostcallplugin subprocess twice
// (two distinct plugin IDs, so two distinct real subprocesses) through the same
// pluginproc.Manager + SetHostBuilder wiring boot uses, and drives both purely
// via DriverService.List. Obj "http" and "config" names encode what the
// subprocess observed when it called back into the host.
func TestReverseBridgeProductionPath(t *testing.T) {
	bin := hostcallPluginBin(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	allowedHost := u.Hostname() // "127.0.0.1" — an explicit literal, so hostcap's
	// anti-rebinding gate (computeAllowInternal) also opts this plugin into
	// reaching it despite it being a loopback address.

	t.Setenv("HOSTCALLPLUGIN_TARGET_URL", srv.URL)

	const knownConfig = `{"key":"integration-value"}`

	m := pluginproc.New()
	m.SetHostBuilder(func(pluginID string) ext.Host {
		switch pluginID {
		case "hostcall.allowed":
			return hostcap.New(hostcap.Grant{
				PluginID: pluginID,
				HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapNetwork}},
				Network:  []string{allowedHost},
			}, hostcap.Options{
				OutboundPolicy: hostcap.AllowAllOutbound,
				ConfigLoader:   func() ([]byte, error) { return []byte(knownConfig), nil },
			})
		case "hostcall.denied":
			// Granted the network capability, but the allowlist names a host
			// other than the test server — the deny case must come from Grant,
			// not from missing the capability entirely.
			return hostcap.New(hostcap.Grant{
				PluginID: pluginID,
				HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapNetwork}},
				Network:  []string{"off-allowlist.example.com"},
			}, hostcap.Options{
				OutboundPolicy: hostcap.AllowAllOutbound,
				ConfigLoader:   func() ([]byte, error) { return []byte(knownConfig), nil },
			})
		default:
			return nil
		}
	})

	// --- allowed plugin: real subprocess #1 ---
	allowedDrv, releaseAllowed, err := m.Acquire(ctx, "hostcall.allowed", bin)
	if err != nil {
		t.Fatalf("acquire hostcall.allowed: %v", err)
	}
	defer releaseAllowed()

	objs, err := allowedDrv.List(ctx, ext.ObjRef{}, ext.ListArgs{})
	if err != nil {
		t.Fatalf("List (allowed): %v", err)
	}
	httpObj, cfgObj := findObj(t, objs, "http"), findObj(t, objs, "config")

	// Init ran and handed the subprocess a working host: List did not fall back
	// to the "before Init" sentinel and instead reflects a real HTTP round trip.
	if strings.Contains(httpObj.Name, "before Init") {
		t.Fatalf("List (allowed) ran before Init reached the subprocess: %q", httpObj.Name)
	}
	wantHTTP := fmt.Sprintf("http=%d", http.StatusOK)
	if httpObj.Name != wantHTTP {
		t.Fatalf("http obj (allowed) = %q, want %q — host.HTTP().Do should have reached the allowed test server", httpObj.Name, wantHTTP)
	}
	wantConfig := "config=" + knownConfig
	if cfgObj.Name != wantConfig {
		t.Fatalf("config obj (allowed) = %q, want %q — host.LoadConfig should have returned the installed ConfigLoader value", cfgObj.Name, wantConfig)
	}

	// --- denied plugin: real subprocess #2, same binary, different grant ---
	deniedDrv, releaseDenied, err := m.Acquire(ctx, "hostcall.denied", bin)
	if err != nil {
		t.Fatalf("acquire hostcall.denied: %v", err)
	}
	defer releaseDenied()

	if m.Count() != 2 {
		t.Fatalf("live subprocess count = %d, want 2 (one per pluginID)", m.Count())
	}

	objs, err = deniedDrv.List(ctx, ext.ObjRef{}, ext.ListArgs{})
	if err != nil {
		t.Fatalf("List (denied): %v", err)
	}
	httpObj, cfgObj = findObj(t, objs, "http"), findObj(t, objs, "config")

	if !strings.HasPrefix(httpObj.Name, "http=denied:") {
		t.Fatalf("http obj (denied) = %q, want prefix %q — host.HTTP().Do should have refused a target off this plugin's Grant.Network", httpObj.Name, "http=denied:")
	}
	// LoadConfig is orthogonal to the network grant — it still delivers the
	// same installed config even though HTTP was refused.
	if cfgObj.Name != wantConfig {
		t.Fatalf("config obj (denied) = %q, want %q", cfgObj.Name, wantConfig)
	}
}

func findObj(t *testing.T, objs []ext.Obj, id string) ext.Obj {
	t.Helper()
	for _, o := range objs {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("List result missing Obj with ID %q (got %d objs)", id, len(objs))
	return ext.Obj{}
}
