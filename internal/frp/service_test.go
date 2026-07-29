package frp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/extension"
	toml "github.com/pelletier/go-toml/v2"
)

// frpProcessWait bounds every wait in this package's tests that depends on a
// real OS process being scheduled, exec'd, and (for the fake-frpc scripts)
// writing to a marker file, or on a subprocess actually being killed and
// reaped after a context cancel. 2-3s was fine running this package alone,
// but a full-repo `go test -race ./...` run reproduced two real timeouts here
// ("fake frpc never recorded its argv" / "recorded 0 starts, want >= 1") with
// NO data race reported alongside them -- i.e. a genuine scheduling-latency
// flake under heavy machine load (every package's test binary running
// -race-instrumented and in parallel), not the shared-loadConfig race
// Service.configLoader fixed. The assertions these deadlines gate (marker
// must show >= N starts, state must reach X) are unchanged; only the budget
// for real process scheduling to catch up is more generous, so this doesn't
// paper over a race -- it just stops a slow-but-correct run from being
// misread as a failure.
const frpProcessWait = 10 * time.Second

// newTestService builds a Service with an isolated per-instance config
// loader, instead of mutating the shared package-level loadConfig var (see
// Service.configLoader's doc comment in service.go for why the latter used to
// be a genuine, -race-confirmed data race between one test's leftover Run
// goroutine and a later test's cleanup).
func newTestService(c frpConfig) *Service {
	return &Service{
		state:        ext.ServiceStateStopped,
		configLoader: func() frpConfig { return c },
	}
}

// useTempDataDir points flags.DataDir at a throwaway dir so Run never writes to
// the real data directory.
func useTempDataDir(t *testing.T) {
	t.Helper()
	prev := flags.DataDir
	flags.DataDir = t.TempDir()
	t.Cleanup(func() { flags.DataDir = prev })
}

func TestFrpConfigValidate(t *testing.T) {
	// missing server addr → fails.
	if err := (frpConfig{RemotePort: 6000}).validate(); err == nil {
		t.Fatal("missing server_addr: want error, got nil")
	}
	// missing remote port → fails.
	if err := (frpConfig{ServerAddr: "frps.example.com"}).validate(); err == nil {
		t.Fatal("missing remote_port: want error, got nil")
	}
	// complete → ok.
	if err := (frpConfig{ServerAddr: "frps.example.com", RemotePort: 6000}).validate(); err != nil {
		t.Fatalf("complete config: want nil, got %v", err)
	}
}

func TestFrpTomlEncodesAndEscapes(t *testing.T) {
	c := frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      "s3cr3t",
		RemotePort: 6000,
		Protocol:   "tcp",
		LocalPort:  5244,
	}
	out, err := renderFrpcToml(c)
	if err != nil {
		t.Fatalf("renderFrpcToml: %v", err)
	}
	s := string(out)
	for _, want := range []string{"serverAddr", "serverPort", "token", "localPort", "remotePort", "s3cr3t", "5244", "6000"} {
		if !strings.Contains(s, want) {
			t.Fatalf("rendered toml missing %q:\n%s", want, s)
		}
	}

	// Injection attempt: a malicious server address containing a quote + newline +
	// a fake toml assignment must be escaped by the encoder, not break out into a
	// top-level `injected` key.
	evil := frpConfig{
		Enabled:    true,
		ServerAddr: "evil\"\ninjected = true",
		ServerPort: 7000,
		RemotePort: 6000,
		Protocol:   "tcp",
		LocalPort:  5244,
	}
	evilOut, err := renderFrpcToml(evil)
	if err != nil {
		t.Fatalf("renderFrpcToml(evil): %v", err)
	}
	var decoded map[string]any
	if err := toml.Unmarshal(evilOut, &decoded); err != nil {
		t.Fatalf("re-parse rendered toml: %v", err)
	}
	if _, ok := decoded["injected"]; ok {
		t.Fatalf("injection escaped: top-level 'injected' key present:\n%s", string(evilOut))
	}
	if decoded["serverAddr"] != "evil\"\ninjected = true" {
		t.Fatalf("serverAddr round-trip mismatch: got %#v", decoded["serverAddr"])
	}
}

func TestFrpRunDisabledIdles(t *testing.T) {
	useTempDataDir(t)
	s := newTestService(frpConfig{Enabled: false})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give Run a moment to settle into the disabled idle state.
	time.Sleep(50 * time.Millisecond)
	st, _ := s.Status(context.Background())
	if st.State != ext.ServiceStateStopped {
		t.Fatalf("disabled: want stopped, got %q", st.State)
	}
	// No frpc.toml should have been written (no process path taken).
	if _, err := os.Stat(filepath.Join(flags.DataDir, "frp", "frpc.toml")); !os.IsNotExist(err) {
		t.Fatalf("disabled must not write frpc.toml, stat err=%v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on cancel: %v", err)
		}
	case <-time.After(frpProcessWait):
		t.Fatal("Run did not return after cancel")
	}
}

func TestFrpRunMissingBinary(t *testing.T) {
	useTempDataDir(t)
	s := newTestService(frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist-frpc"),
		LocalPort:  5244,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	st, _ := s.Status(context.Background())
	if st.State != ext.ServiceStateError {
		t.Fatalf("missing binary: want error, got %q", st.State)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on cancel: %v", err)
		}
	case <-time.After(frpProcessWait):
		t.Fatal("Run did not return after cancel")
	}
}

func TestFrpRunStartsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	// A fake frpc: accepts `-c <path>` and sleeps, so Run stays "running" until
	// the context kills it.
	bin := filepath.Join(t.TempDir(), "fake-frpc")
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	s := newTestService(frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: bin,
		LocalPort:  5244,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait until Run reports running.
	deadline := time.Now().Add(frpProcessWait)
	for {
		st, _ := s.Status(context.Background())
		if st.State == ext.ServiceStateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service never reached running, last state=%q", st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// frpc.toml was written with 0600 perms.
	info, err := os.Stat(filepath.Join(flags.DataDir, "frp", "frpc.toml"))
	if err != nil {
		t.Fatalf("frpc.toml not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("frpc.toml perm = %o, want 0600", perm)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(frpProcessWait):
		t.Fatal("Run did not return after cancel (process not killed?)")
	}
	st, _ := s.Status(context.Background())
	if st.State != ext.ServiceStateStopped {
		t.Fatalf("after cancel: want stopped, got %q", st.State)
	}
}

// waitState polls the service until it reaches want or the deadline elapses.
func waitState(t *testing.T, s *Service, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		st, _ := s.Status(context.Background())
		if st.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("service never reached %q, last state=%q", want, st.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// countStarts returns how many times the fake frpc recorded a start.
func countStarts(t *testing.T, marker string) int {
	t.Helper()
	b, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read start marker: %v", err)
	}
	return strings.Count(string(b), "start\n")
}

// waitStarts polls the marker until the fake frpc has recorded at least n starts.
// The running state is set just before exec, so the script's own write lands a
// beat later — callers wait on the marker, not the state, to observe a respawn.
func waitStarts(t *testing.T, marker string, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if countStarts(t, marker) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake frpc recorded %d starts, want >= %d", countStarts(t, marker), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeFrpcConfig writes a fake frpc that records each start into marker then
// sleeps, and returns a config pointing at it. Each Reload-driven restart appends
// another "start" line, so the test can prove the process was actually respawned.
func fakeFrpcConfig(t *testing.T, marker string) frpConfig {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-frpc")
	script := "#!/bin/sh\necho start >> " + marker + "\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: bin,
		LocalPort:  5244,
	}
}

// TestFrpReloadReconnects proves a config change (Reload) kills the running frpc
// and re-enters the loop with fresh config, respawning the process — the tunnel
// reconnects without the whole Run returning.
func TestFrpReloadReconnects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	marker := filepath.Join(t.TempDir(), "starts")
	s := newTestService(fakeFrpcConfig(t, marker))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)
	waitStarts(t, marker, 1, frpProcessWait) // fake frpc recorded its first start

	// Hot-reload: current frpc is killed, loop re-enters, new frpc starts.
	s.Reload()
	waitStarts(t, marker, 2, frpProcessWait) // respawned with fresh config
	// Run kept supervising and settled back into running with the new session.
	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(frpProcessWait):
		t.Fatal("Run did not return after cancel")
	}
}

// TestFrpRunLoopStopsOnCtxCancel proves the inner reconnect loop terminates when
// the supervised ctx is cancelled — even after a Reload cycle — so shutdown never
// hangs on the loop.
func TestFrpRunLoopStopsOnCtxCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	marker := filepath.Join(t.TempDir(), "starts")
	s := newTestService(fakeFrpcConfig(t, marker))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)
	waitStarts(t, marker, 1, frpProcessWait)
	// Exercise a reload so the loop has re-entered at least once before cancel.
	s.Reload()
	waitStarts(t, marker, 2, frpProcessWait)
	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after ctx cancel: %v", err)
		}
	case <-time.After(frpProcessWait):
		t.Fatal("Run loop did not terminate on ctx cancel")
	}
	waitState(t, s, ext.ServiceStateStopped, frpProcessWait)
}

// TestFrpStatusViaExtensionHandler proves the status-card link works end to end:
// registering the built-in service makes ServiceStatusOf resolve the frp Status
// (the same seam the /status/{sid} handler calls), and the Manifest advertises a
// StatusProvider whose PollPath targets that route.
func TestFrpStatusViaExtensionHandler(t *testing.T) {
	useTempDataDir(t)

	// Register a fresh instance into an isolated registry (never the process
	// DefaultRegistry) so the test does not depend on boot order or leak state.
	// svc.Run is never invoked here (registration only wires Manifest/Status),
	// so it doesn't need a configLoader.
	svc := newTestService(frpConfig{Enabled: false})
	r := extension.NewRegistry()
	if err := extension.RegisterBuiltinBackgroundService(r, ServiceID, svc); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { extension.RegisterBackgroundServiceInstance(ServiceID, nil) })

	// The Manifest carried into the registry advertises the StatusProvider card
	// pointing at the poll route.
	e, ok := r.Get(ServiceID)
	if !ok {
		t.Fatal("frp entry not registered")
	}
	sp := e.Manifest.Contributions.StatusProviders
	if len(sp) != 1 {
		t.Fatalf("want 1 StatusProvider, got %d", len(sp))
	}
	wantPath := "/api/v4/extensions/" + ServiceID + "/status/" + sp[0].ID
	if sp[0].PollPath != wantPath {
		t.Fatalf("StatusProvider PollPath = %q, want %q", sp[0].PollPath, wantPath)
	}

	// ServiceStatusOf is exactly what the status handler calls; it must resolve
	// the frp service and return its (disabled → stopped) status.
	st, err := extension.ServiceStatusOf(context.Background(), r, ServiceID)
	if err != nil {
		t.Fatalf("ServiceStatusOf: %v", err)
	}
	if st.State != ext.ServiceStateStopped {
		t.Fatalf("status via handler seam: want stopped, got %q", st.State)
	}
}

// TestFrpManifestDeclaresSettingsPage pins the C6 contract: the frp service
// Manifest declares a native (/@manage/) settings page routing to the generic
// FeatureConfigPage (/@manage/feature/builtin.frp) ALONGSIDE its existing
// StatusProvider card — the settings page must be merged into Contributions, not
// overwrite the status provider. The route moved off the raw FRP SettingList
// (/@manage/setting/frp) so the extension-manager link lands on the FRP feature
// page (general config section + live status card) rather than a duplicate
// top-level settings group (design C6 去重).
func TestFrpManifestDeclaresSettingsPage(t *testing.T) {
	s := &Service{state: ext.ServiceStateStopped}
	c := s.Manifest().Contributions

	// The pre-existing StatusProvider must survive the merge.
	if len(c.StatusProviders) != 1 {
		t.Fatalf("frp Manifest must keep its StatusProvider, got %d", len(c.StatusProviders))
	}

	if len(c.SettingsPages) != 1 {
		t.Fatalf("frp Manifest must declare exactly one settings page, got %+v", c.SettingsPages)
	}
	p := c.SettingsPages[0]
	if p.ID != "frp" {
		t.Errorf("frp settings page id = %q, want %q", p.ID, "frp")
	}
	if p.Route != "/@manage/feature/"+ServiceID {
		t.Errorf("frp settings page route = %q, want %q", p.Route, "/@manage/feature/"+ServiceID)
	}
	if p.Title["en"] != "FRP" || p.Title["zh"] != "内网穿透" {
		t.Errorf("frp settings page title = %+v, want FRP/内网穿透", p.Title)
	}
}

// TestFrpManifestDeclaresConfigSection pins the C6 contract: the frp service
// Manifest declares a ConfigSection with Parent=self (builtin.frp) carrying the
// seven seeded FRP setting keys in seed order — this is the "general" section the
// FeatureConfigPage(builtin.frp) renders. It must be merged ALONGSIDE the
// StatusProvider and SettingsPages, and surface through AggregateContributions.
func TestFrpManifestDeclaresConfigSection(t *testing.T) {
	s := &Service{state: ext.ServiceStateStopped}
	c := s.Manifest().Contributions

	// The pre-existing StatusProvider must survive the merge.
	if len(c.StatusProviders) != 1 {
		t.Fatalf("frp Manifest must keep its StatusProvider, got %d", len(c.StatusProviders))
	}
	if len(c.ConfigSections) != 1 {
		t.Fatalf("frp Manifest must declare exactly one config section, got %+v", c.ConfigSections)
	}
	sec := c.ConfigSections[0]
	if sec.Parent != ServiceID {
		t.Errorf("frp config section Parent = %q, want %q (self)", sec.Parent, ServiceID)
	}
	if sec.Title["en"] != "General" || sec.Title["zh"] != "通用配置" {
		t.Errorf("frp config section title = %+v, want General/通用配置", sec.Title)
	}
	if sec.Order != 0 {
		t.Errorf("frp config section order = %d, want 0", sec.Order)
	}
	wantKeys := []string{
		conf.FrpEnabled,
		conf.FrpServerAddr,
		conf.FrpServerPort,
		conf.FrpToken,
		conf.FrpRemotePort,
		conf.FrpProtocol,
		conf.FrpBinaryPath,
	}
	if len(sec.SettingKeys) != len(wantKeys) {
		t.Fatalf("frp config section keys = %v, want %v", sec.SettingKeys, wantKeys)
	}
	for i, k := range wantKeys {
		if sec.SettingKeys[i] != k {
			t.Errorf("frp config section key[%d] = %q, want %q", i, sec.SettingKeys[i], k)
		}
	}

	// The section flows through AggregateContributions unchanged when the service
	// is registered and enabled.
	r := extension.NewRegistry()
	if err := extension.RegisterBuiltinBackgroundService(r, ServiceID, s); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { extension.RegisterBackgroundServiceInstance(ServiceID, nil) })
	doc := extension.AggregateContributions(r)
	var found []ext.ConfigSection
	for _, e := range doc.Extensions {
		if e.ID == ServiceID {
			for _, cs := range e.Contributions.ConfigSections {
				if cs.Parent == ServiceID {
					found = append(found, cs)
				}
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("AggregateContributions must surface the frp general section, got %+v", found)
	}
}

func TestFrpStatusNoToken(t *testing.T) {
	useTempDataDir(t)
	const secret = "secret123"
	s := newTestService(frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      secret,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist-frpc"), // stays in error state, no process
		LocalPort:  5244,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	st, _ := s.Status(context.Background())
	blob, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("token leaked into status: %s", blob)
	}
	if strings.Contains(st.Detail, secret) {
		t.Fatalf("token leaked into Detail: %q", st.Detail)
	}

	cancel()
	<-done
}
