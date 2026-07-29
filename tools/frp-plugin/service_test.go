package main

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
	toml "github.com/pelletier/go-toml/v2"
)

// withConfig overrides the package config loader for one test.
func withConfig(t *testing.T, c frpConfig) {
	t.Helper()
	prev := loadConfig
	loadConfig = func() frpConfig { return c }
	t.Cleanup(func() { loadConfig = prev })
}

// TestLoadEnvConfig proves config is read from the injected environment with the
// documented defaults, and that validate() rejects an incomplete config.
func TestLoadEnvConfig(t *testing.T) {
	// Defaults when only the required fields are set.
	t.Setenv("FRP_SERVER_ADDR", "frps.example.com")
	t.Setenv("FRP_REMOTE_PORT", "6000")
	c := loadEnvConfig()
	if c.ServerAddr != "frps.example.com" {
		t.Fatalf("ServerAddr = %q, want frps.example.com", c.ServerAddr)
	}
	if c.RemotePort != 6000 {
		t.Fatalf("RemotePort = %d, want 6000", c.RemotePort)
	}
	if c.ServerPort != 7000 {
		t.Fatalf("ServerPort default = %d, want 7000", c.ServerPort)
	}
	if c.Protocol != "tcp" {
		t.Fatalf("Protocol default = %q, want tcp", c.Protocol)
	}
	if c.BinaryPath != "frpc" {
		t.Fatalf("BinaryPath default = %q, want frpc", c.BinaryPath)
	}
	if c.LocalPort != 5244 {
		t.Fatalf("LocalPort default = %d, want 5244", c.LocalPort)
	}
	if err := c.validate(); err != nil {
		t.Fatalf("complete config: want nil, got %v", err)
	}

	// Overrides.
	t.Setenv("FRP_SERVER_PORT", "7001")
	t.Setenv("FRP_TOKEN", "s3cr3t")
	t.Setenv("FRP_PROTOCOL", "kcp")
	t.Setenv("FRP_BINARY_PATH", "/opt/frpc")
	t.Setenv("FRP_LOCAL_PORT", "8080")
	c = loadEnvConfig()
	if c.ServerPort != 7001 || c.Token != "s3cr3t" || c.Protocol != "kcp" ||
		c.BinaryPath != "/opt/frpc" || c.LocalPort != 8080 {
		t.Fatalf("env overrides not applied: %+v", c)
	}
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

func TestFrpRunInvalidConfigIdles(t *testing.T) {
	// No server addr → validate fails → error idle, parked on ctx.Done.
	withConfig(t, frpConfig{RemotePort: 6000})
	s := newFrpService()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	st, _ := s.Status(context.Background())
	if st.State != ext.ServiceStateError {
		t.Fatalf("invalid config: want error, got %q", st.State)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestFrpRunMissingBinary(t *testing.T) {
	withConfig(t, frpConfig{
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist-frpc"),
		LocalPort:  5244,
	})
	s := newFrpService()

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
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestFrpRunStartsAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is POSIX-only")
	}
	// A fake frpc: accepts `-c <path>` and sleeps, so Run stays "running" until
	// the context kills it.
	bin := filepath.Join(t.TempDir(), "fake-frpc")
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	withConfig(t, frpConfig{
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: bin,
		LocalPort:  5244,
	})
	s := newFrpService()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait until Run reports running.
	deadline := time.Now().Add(2 * time.Second)
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
	// frpc.toml was written with 0600 perms in the plugin temp dir.
	tomlPath := filepath.Join(os.TempDir(), "alist-frp-plugin", "frpc.toml")
	info, err := os.Stat(tomlPath)
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
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel (process not killed?)")
	}
	st, _ := s.Status(context.Background())
	if st.State != ext.ServiceStateStopped {
		t.Fatalf("after cancel: want stopped, got %q", st.State)
	}
}

func TestFrpStatusNoToken(t *testing.T) {
	const secret = "secret123"
	withConfig(t, frpConfig{
		ServerAddr: "frps.example.com",
		ServerPort: 7000,
		Token:      secret,
		RemotePort: 6000,
		Protocol:   "tcp",
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist-frpc"), // stays in error state, no process
		LocalPort:  5244,
	})
	s := newFrpService()

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

// TestManifestMatchesPluginJSON proves the Go Manifest() and the shipped
// plugin.json are JSON-equivalent on the fields the host reads: an install
// package's plugin.json and the running binary's self-reported Manifest must
// agree, or registration would mismatch. We compare through a normalized
// map[string]any so key order / whitespace never matter.
func TestManifestMatchesPluginJSON(t *testing.T) {
	// The Go manifest, round-tripped through JSON into a generic map.
	mj, err := json.Marshal(newFrpService().Manifest())
	if err != nil {
		t.Fatalf("marshal Manifest: %v", err)
	}
	var fromGo map[string]any
	if err := json.Unmarshal(mj, &fromGo); err != nil {
		t.Fatalf("unmarshal Manifest json: %v", err)
	}

	// The shipped plugin.json, into a generic map.
	pjBytes, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var fromFile map[string]any
	if err := json.Unmarshal(pjBytes, &fromFile); err != nil {
		t.Fatalf("unmarshal plugin.json: %v", err)
	}

	// Compare the load-bearing top-level fields verbatim.
	for _, key := range []string{"id", "name", "version", "core_api", "kinds", "provenance", "permissions", "contributions"} {
		g, _ := json.Marshal(fromGo[key])
		f, _ := json.Marshal(fromFile[key])
		if string(g) != string(f) {
			t.Fatalf("field %q mismatch:\n Manifest(): %s\n plugin.json: %s", key, g, f)
		}
	}
}
