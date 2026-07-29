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
	toml "github.com/pelletier/go-toml/v2"
)

// This file holds adversarial (red-team) regressions for the FRP tunnel MVP.
// The three headline properties under attack: (1) the frps token never leaks
// (argv / Status / toml perms), (2) malicious setting values cannot inject
// extra toml or shell commands, and (3) the supervised process is robust — it
// never panics and never busy-loops on a bad config / missing binary.

// --- 1. token leakage --------------------------------------------------------

// TestFrpArgvNeverCarriesToken proves the token travels through the 0600 toml
// file only — never the process argv. The fake frpc records its own arguments;
// the recorded argv must contain `-c <toml>` but NOT the secret token.
//
// LOAD-BEARING: if service.go ever splices the token into the command line
// (e.g. `exec.CommandContext(ctx, bin, "--token", c.Token, ...)`) the recorded
// argv would contain the secret and this test turns red.
func TestFrpArgvNeverCarriesToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	const secret = "argv-should-never-see-this-token"
	argMarker := filepath.Join(t.TempDir(), "argv")
	bin := filepath.Join(t.TempDir(), "fake-frpc")
	// Record the full argv (including $0) then block so Run stays running.
	script := "#!/bin/sh\necho \"$0 $@\" > " + argMarker + "\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	s := newTestService(frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      secret,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: bin,
		LocalPort:  5244,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)

	// Wait for the fake frpc to record its argv.
	deadline := time.Now().Add(frpProcessWait)
	var argv string
	for {
		b, err := os.ReadFile(argMarker)
		if err == nil && len(b) > 0 {
			argv = string(b)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake frpc never recorded its argv")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(argv, secret) {
		t.Fatalf("token leaked into frpc argv: %q", argv)
	}
	if !strings.Contains(argv, "-c") {
		t.Fatalf("expected fixed `-c <path>` argv, got %q", argv)
	}
}

// TestFrpTomlFilePermTightenedOnPreexisting proves the token file is always
// 0600, even when frpc.toml already exists with wider permissions. os.WriteFile
// only applies its mode when it CREATES the file — a pre-existing looser file
// keeps its old bits — so an attacker who can pre-create <data>/frp/frpc.toml
// as world-readable could otherwise read the token alist writes into it.
//
// LOAD-BEARING: delete the explicit os.Chmod(tomlPath, 0600) in service.go and
// this test turns red (the file keeps its pre-seeded 0644).
func TestFrpTomlFilePermTightenedOnPreexisting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	// Pre-seed frp/frpc.toml with world/group-readable perms.
	dir := filepath.Join(flags.DataDir, "frp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tomlPath := filepath.Join(dir, "frpc.toml")
	if err := os.WriteFile(tomlPath, []byte("stale = true\n"), 0644); err != nil {
		t.Fatalf("seed toml: %v", err)
	}
	if err := os.Chmod(tomlPath, 0644); err != nil { // defeat umask on the seed
		t.Fatalf("chmod seed: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "fake-frpc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	s := newTestService(frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      "s3cr3t",
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: bin,
		LocalPort:  5244,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)

	info, err := os.Stat(tomlPath)
	if err != nil {
		t.Fatalf("stat toml: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("token file perm = %o, want 0600 even when pre-existing", perm)
	}
}

// TestFrpDataDirPerm proves the frp data dir is 0700 (owner-only), so the token
// file cannot be reached by other local users through the directory.
func TestFrpDataDirPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	bin := filepath.Join(t.TempDir(), "fake-frpc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	s := newTestService(frpConfig{
		Enabled: true, ServerAddr: "frps.example.com", ServerPort: 7000,
		Token: "s3cr3t", RemotePort: 6000, Protocol: "tcp", BinaryPath: bin, LocalPort: 5244,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitState(t, s, ext.ServiceStateRunning, frpProcessWait)
	info, err := os.Stat(filepath.Join(flags.DataDir, "frp"))
	if err != nil {
		t.Fatalf("stat frp dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Fatalf("frp data dir perm = %o, want 0700", perm)
	}
}

// TestFrpStatusNeverCarriesTokenAcrossStates sweeps every state the service can
// report — disabled, invalid-config, missing-binary — and asserts the token
// never appears in the JSON-serialized Status (Detail + Metrics). W1's
// TestFrpStatusNoToken only covered the missing-binary error state.
func TestFrpStatusNeverCarriesTokenAcrossStates(t *testing.T) {
	const secret = "state-sweep-secret-42"
	base := frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      secret,
		RemotePort: 6000,
		Protocol:   "tcp",
		LocalPort:  5244,
	}
	cases := map[string]frpConfig{
		"disabled":       func() frpConfig { c := base; c.Enabled = false; return c }(),
		"invalid-config": func() frpConfig { c := base; c.ServerAddr = ""; return c }(),
		"missing-binary": func() frpConfig { c := base; c.BinaryPath = filepath.Join(t.TempDir(), "nope-frpc"); return c }(),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			useTempDataDir(t)
			s := newTestService(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })

			time.Sleep(60 * time.Millisecond)
			st, _ := s.Status(context.Background())
			blob, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("marshal status: %v", err)
			}
			if strings.Contains(string(blob), secret) {
				t.Fatalf("token leaked into %s status: %s", name, blob)
			}
		})
	}
}

// --- 2. toml / command injection ---------------------------------------------

// TestFrpTomlInjectionHardened attacks renderFrpcToml with malicious values in
// every string field (server address, token, protocol) containing quotes,
// newlines and fake `[section]` / assignments. The go-toml encoder must escape
// them so a re-parse yields exactly the intended document — no injected
// top-level keys, no extra proxies, and the loopback localIP is preserved.
//
// LOAD-BEARING (mutation counter-proof): rewrite renderFrpcToml with string
// concatenation / fmt.Sprintf instead of the struct encoder and these round-trip
// assertions turn red — proving structured encoding is the injection defense.
func TestFrpTomlInjectionHardened(t *testing.T) {
	evil := frpConfig{
		Enabled:    true,
		ServerAddr: "evil\"\ninjected_addr = true\n[proxies.attacker]\nname = \"x",
		ServerPort: 7000,
		Token:      "tok\"\ninjected_token = true\nserverAddr = \"attacker.example.com",
		RemotePort: 6000,
		Protocol:   "tcp\"\ninjected_proto = true\n[[proxies]]\nname = \"evil2",
		LocalPort:  5244,
	}
	out, err := renderFrpcToml(evil)
	if err != nil {
		t.Fatalf("renderFrpcToml(evil): %v", err)
	}

	// Decode into the real schema and verify no field bled into another and no
	// extra proxy appeared.
	var doc frpcToml
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if doc.ServerAddr != evil.ServerAddr {
		t.Fatalf("serverAddr round-trip mismatch: got %#v", doc.ServerAddr)
	}
	if doc.Auth.Token != evil.Token {
		t.Fatalf("token round-trip mismatch: got %#v", doc.Auth.Token)
	}
	if len(doc.Proxies) != 1 {
		t.Fatalf("injection created extra proxies: got %d, want 1", len(doc.Proxies))
	}
	if doc.Proxies[0].LocalIP != "127.0.0.1" {
		t.Fatalf("localIP must stay loopback, got %q", doc.Proxies[0].LocalIP)
	}
	if doc.Proxies[0].Type != evil.Protocol {
		t.Fatalf("protocol round-trip mismatch: got %#v", doc.Proxies[0].Type)
	}

	// Decode into a loose map: no attacker-chosen top-level key must exist.
	var loose map[string]any
	if err := toml.Unmarshal(out, &loose); err != nil {
		t.Fatalf("re-parse loose: %v", err)
	}
	for _, k := range []string{"injected_addr", "injected_token", "injected_proto", "attacker"} {
		if _, ok := loose[k]; ok {
			t.Fatalf("injection escaped: top-level %q present:\n%s", k, out)
		}
	}
}

// TestFrpTomlExposesLoopbackOnly locks the exposure surface: the single proxy
// maps 127.0.0.1:LocalPort (alist itself) — never 0.0.0.0 or another host — to
// the frps remote port.
func TestFrpTomlExposesLoopbackOnly(t *testing.T) {
	out, err := renderFrpcToml(frpConfig{
		Enabled: true, ServerAddr: "frps.example.com", ServerPort: 7000,
		RemotePort: 6000, Protocol: "tcp", LocalPort: 5244,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var doc frpcToml
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(doc.Proxies) != 1 {
		t.Fatalf("want exactly 1 proxy, got %d", len(doc.Proxies))
	}
	if doc.Proxies[0].LocalIP != "127.0.0.1" {
		t.Fatalf("localIP = %q, want 127.0.0.1 (must not bind 0.0.0.0)", doc.Proxies[0].LocalIP)
	}
	if strings.Contains(string(out), "0.0.0.0") {
		t.Fatalf("rendered toml must never contain 0.0.0.0:\n%s", out)
	}
	if doc.Proxies[0].LocalPort != 5244 {
		t.Fatalf("localPort = %d, want the alist HTTP port 5244", doc.Proxies[0].LocalPort)
	}
}

// TestFrpBinaryPathNotShellInterpreted proves the binary path is passed to
// exec.CommandContext as a single executable name, never through a shell. A
// path containing shell metacharacters (`;`, `$()`) is treated as one literal
// filename: LookPath fails, the service parks in the error state, and no side
// effect (the `touch`ed sentinel) is ever produced.
func TestFrpBinaryPathNotShellInterpreted(t *testing.T) {
	useTempDataDir(t)
	sentinel := filepath.Join(t.TempDir(), "pwned")
	s := newTestService(frpConfig{
		Enabled:    true,
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      "s3cr3t",
		RemotePort: 6000,
		Protocol:   "tcp",
		// If this were handed to a shell, the `;` would spawn a second command.
		BinaryPath: "/nonexistent-frpc; touch " + sentinel,
		LocalPort:  5244,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitState(t, s, ext.ServiceStateError, frpProcessWait)
	// Give any (erroneously) spawned shell command a moment to land.
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("binary path was shell-interpreted: injected `touch` ran")
	}
}

// --- 3. robustness / no busy-loop --------------------------------------------

// TestFrpIdleStatesBlockNotBusyLoop is the anti-busy-loop guard. In every
// non-fatal-but-cannot-run state (disabled / invalid-config / missing-binary)
// runOnce MUST park on <-runCtx.Done() rather than return — otherwise the outer
// Run loop would spin, re-rendering and hammering setState in a tight loop
// (a CPU/log DoS). We assert runOnce does NOT return while runCtx is live, then
// returns promptly (nil) once it is cancelled.
//
// LOAD-BEARING: replace any idle `<-runCtx.Done()` in runOnce with a bare
// `return` and the "must not return while ctx is live" assertion turns red.
func TestFrpIdleStatesBlockNotBusyLoop(t *testing.T) {
	cases := map[string]frpConfig{
		"disabled":       {Enabled: false},
		"invalid-config": {Enabled: true, ServerAddr: "", RemotePort: 6000, Protocol: "tcp", LocalPort: 5244},
		"missing-binary": {Enabled: true, ServerAddr: "frps.example.com", ServerPort: 7000, RemotePort: 6000, Protocol: "tcp", BinaryPath: filepath.Join(t.TempDir(), "absent-frpc"), LocalPort: 5244},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			useTempDataDir(t)
			s := newTestService(cfg)

			runCtx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.runOnce(runCtx) }()

			// It must still be parked (idle) a beat later — not busy-returning.
			select {
			case err := <-done:
				t.Fatalf("runOnce returned early (busy-loop risk) for %s: err=%v", name, err)
			case <-time.After(150 * time.Millisecond):
			}

			// Once the session ctx is cancelled it must wake and return nil
			// (idle state, not a supervisor-backoff error).
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: idle runOnce must return nil on cancel, got %v", name, err)
				}
			case <-time.After(frpProcessWait):
				t.Fatalf("%s: runOnce did not return after cancel", name)
			}
		})
	}
}

// TestFrpFastExitingBinaryReturnsErrForBackoff proves that when frpc exits on
// its own (e.g. frps rejected the config), runOnce returns the error so the
// supervisor backs off — rather than an internal tight retry. Distinct from the
// idle states above: a real process exit is the ONE case that returns non-nil.
func TestFrpFastExitingBinaryReturnsErrForBackoff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	useTempDataDir(t)
	bin := filepath.Join(t.TempDir(), "fake-frpc")
	// Exit non-zero immediately, like frpc rejecting a bad config.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	s := newTestService(frpConfig{
		Enabled: true, ServerAddr: "frps.example.com", ServerPort: 7000,
		Token: "s3cr3t", RemotePort: 6000, Protocol: "tcp", BinaryPath: bin, LocalPort: 5244,
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.runOnce(runCtx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fast-exiting frpc must return an error so the supervisor backs off")
		}
	case <-time.After(frpProcessWait):
		t.Fatal("runOnce did not return after the fake frpc exited")
	}
	st, _ := s.Status(context.Background())
	if st.State != ext.ServiceStateError {
		t.Fatalf("after process exit: want error state, got %q", st.State)
	}
}
