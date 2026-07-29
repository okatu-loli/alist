package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestClientConfigRoundTrip pins the basic save/load contract.
func TestClientConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := ClientConfig{Host: "http://localhost:5244", AgentID: "abc123"}
	if err := SaveClientConfig(dir, want); err != nil {
		t.Fatalf("SaveClientConfig: %v", err)
	}
	got, err := LoadClientConfig(dir)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if *got != want {
		t.Fatalf("round trip mismatch: got %+v, want %+v", *got, want)
	}
}

// TestClientConfigRoundTripWithServerPubKey pins design R1: a pinned
// server_pubkey survives the save/load round trip alongside host + agent_id.
func TestClientConfigRoundTripWithServerPubKey(t *testing.T) {
	dir := t.TempDir()
	want := ClientConfig{Host: "http://localhost:5244", AgentID: "abc123", ServerPubKey: "c2VydmVyLXB1YmtleQ=="}
	if err := SaveClientConfig(dir, want); err != nil {
		t.Fatalf("SaveClientConfig: %v", err)
	}
	got, err := LoadClientConfig(dir)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	if *got != want {
		t.Fatalf("round trip mismatch: got %+v, want %+v", *got, want)
	}
	if got.ServerPubKey != want.ServerPubKey {
		t.Fatalf("ServerPubKey not persisted: got %q, want %q", got.ServerPubKey, want.ServerPubKey)
	}
}

// TestClientConfigPersistsAtRestrictivePermissions pins 0600 on the config
// file too — host/agent_id aren't secret, but consistent handling avoids a
// footgun if the shape ever grows a sensitive field.
func TestClientConfigPersistsAtRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix file mode bits don't apply on windows")
	}
	dir := t.TempDir()
	if err := SaveClientConfig(dir, ClientConfig{Host: "h", AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("config file perm = %o, want 0600", perm)
	}
}

// TestLoadClientConfigMissingFileErrors pins the "not paired yet" shape.
func TestLoadClientConfigMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadClientConfig(dir); err == nil {
		t.Fatal("loading a config that was never saved must error")
	}
}

// TestLoadClientConfigIncompleteErrors pins that a config missing agent_id is
// rejected rather than handed to a caller that would then talk to the server
// with an empty agent_id.
func TestLoadClientConfigIncompleteErrors(t *testing.T) {
	dir := t.TempDir()
	if err := SaveClientConfig(dir, ClientConfig{Host: "http://x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadClientConfig(dir); err == nil {
		t.Fatal("config missing agent_id must error")
	}
}

// TestLoadClientConfigReversePairedNoHost pins the reverse-pairing shape
// (design F): a token-paired reverse agent stores agent_id + server_pubkey but
// NO host — it never dials out, the server dials it. Such a config MUST load,
// or a paired `agent serve` silently loses its data listener on every restart
// (the bug this test was written against). Host remains required only on the
// outbound `agent run` path, at its call site.
func TestLoadClientConfigReversePairedNoHost(t *testing.T) {
	dir := t.TempDir()
	want := ClientConfig{AgentID: "abc123", ServerPubKey: "c2VydmVyLXB1YmtleQ=="}
	if err := SaveClientConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClientConfig(dir)
	if err != nil {
		t.Fatalf("a reverse-paired config (empty host) must load, got: %v", err)
	}
	if *got != want {
		t.Fatalf("round trip mismatch: got %+v, want %+v", *got, want)
	}
}
