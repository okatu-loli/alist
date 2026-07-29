package grpc

import (
	"context"
	"testing"

	"github.com/alist-org/alist/v3/internal/extension/hostcap"
)

// This is the W6b reverse-bridge integration gate: a real plugin subprocess
// borrows Host.Store().Secrets() over the plugin->host gRPC bridge (Secret*
// RPCs), mirroring TestReverseBridgeStoreIsolation's Store().KV() coverage —
// round trip, per-plugin isolation, and the CapStore grant gate all hold
// across the real wire, not just in-process.

// TestReverseBridgeSecretsRoundTrip: a plugin sets then gets its own secret
// over the bridge and gets back the original plaintext (the host encrypts on
// its side; the plugin never sees ciphertext).
func TestReverseBridgeSecretsRoundTrip(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()
	dir := t.TempDir()
	key, err := hostcap.LoadOrCreateSecretsKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSecretsKey: %v", err)
	}
	innerMem := hostcap.NewMemKVBackend() // kept so the test can peek at the raw ciphertext
	backend, err := hostcap.NewEncryptingKVBackend(innerMem, key)
	if err != nil {
		t.Fatalf("NewEncryptingKVBackend: %v", err)
	}

	h := hostcap.New(hostcap.Grant{
		PluginID: "it.secretsRT",
		HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}},
	}, hostcap.Options{SecretsBackend: backend})
	id := serveHost(probe, h)

	if _, err := probe.Exercise(ctx, id, "secret.set", "oauth-token", "s3cr3t-value"); err != nil {
		t.Fatalf("secret.set: %v", err)
	}
	if got, err := probe.Exercise(ctx, id, "secret.get", "oauth-token", ""); err != nil || got != "s3cr3t-value" {
		t.Fatalf("secret.get = %q,%v; want s3cr3t-value,nil", got, err)
	}

	// The value stored in the underlying backend is ciphertext, not the
	// plaintext the plugin exchanged with the host over the bridge.
	raw, ok, err := innerMem.Get("it.secretsRT", "oauth-token")
	if err != nil || !ok {
		t.Fatalf("innerMem.Get raw = %v,%v,%v", raw, ok, err)
	}
	if string(raw) == "s3cr3t-value" {
		t.Fatal("secret landed on the backend in plaintext")
	}
}

// TestReverseBridgeSecretsIsolation: two plugins share one Secrets backend;
// plugin B cannot read plugin A's secret (per-plugin isolation, same
// mechanism as Store), and a plugin without CapStore is denied (Secrets
// reuses the Store grant).
func TestReverseBridgeSecretsIsolation(t *testing.T) {
	probe := launchProbe(t)
	ctx := context.Background()
	dir := t.TempDir()
	key, err := hostcap.LoadOrCreateSecretsKey(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateSecretsKey: %v", err)
	}
	backend, err := hostcap.NewEncryptingKVBackend(hostcap.NewMemKVBackend(), key)
	if err != nil {
		t.Fatalf("NewEncryptingKVBackend: %v", err)
	}

	hostA := hostcap.New(hostcap.Grant{PluginID: "it.secretsA", HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}}}, hostcap.Options{SecretsBackend: backend})
	hostB := hostcap.New(hostcap.Grant{PluginID: "it.secretsB", HostAPIs: []hostcap.GrantItem{{Name: hostcap.CapStore}}}, hostcap.Options{SecretsBackend: backend})
	hostC := hostcap.New(hostcap.Grant{PluginID: "it.secretsC"}, hostcap.Options{SecretsBackend: backend}) // no store cap
	idA := serveHost(probe, hostA)
	idB := serveHost(probe, hostB)
	idC := serveHost(probe, hostC)

	if _, err := probe.Exercise(ctx, idA, "secret.set", "k", "A-secret"); err != nil {
		t.Fatalf("A set: %v", err)
	}
	if got, err := probe.Exercise(ctx, idB, "secret.get", "k", ""); err != nil || got != "<missing>" {
		t.Fatalf("B get = %q,%v; want <missing>,nil (isolation)", got, err)
	}
	if got, err := probe.Exercise(ctx, idA, "secret.get", "k", ""); err != nil || got != "A-secret" {
		t.Fatalf("A get = %q,%v; want A-secret", got, err)
	}
	if _, err := probe.Exercise(ctx, idC, "secret.set", "k", "v"); err == nil {
		t.Fatal("C set without CapStore: expected permission error, got nil")
	}
}
