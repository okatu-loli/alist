package hostcap

import (
	"context"
	"strings"
	"sync"

	ext "github.com/AlistGo/alist-ext"
)

// KVBackend is the persistence behind Store. Every method takes the plugin id
// explicitly and the implementation MUST confine the operation to that id's
// namespace — this is where invariant 3 (cross-plugin isolation) is enforced,
// below the point where a plugin could influence the id (the id is a host-side
// fact bound into hostStore, never a wire input).
type KVBackend interface {
	Get(pluginID, key string) (value []byte, ok bool, err error)
	Set(pluginID, key string, value []byte) error
	Delete(pluginID, key string) error
	List(pluginID, prefix string) ([]string, error)
}

// MemKVBackend is an in-memory KVBackend keyed by (pluginID, key). It is the F0
// default; a persistent (db-backed) backend swaps in without touching callers.
// Isolation is structural: each plugin id has its own inner map, so plugin A's
// keys are unreachable from plugin B's namespace.
type MemKVBackend struct {
	mu sync.RWMutex
	m  map[string]map[string][]byte
}

func NewMemKVBackend() *MemKVBackend { return &MemKVBackend{m: map[string]map[string][]byte{}} }

func (b *MemKVBackend) Get(pluginID, key string) ([]byte, bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ns := b.m[pluginID]
	if ns == nil {
		return nil, false, nil
	}
	v, ok := ns[key]
	if !ok {
		return nil, false, nil
	}
	// Return a copy so a caller cannot mutate stored bytes in place.
	out := make([]byte, len(v))
	copy(out, v)
	return out, true, nil
}

func (b *MemKVBackend) Set(pluginID, key string, value []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ns := b.m[pluginID]
	if ns == nil {
		ns = map[string][]byte{}
		b.m[pluginID] = ns
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	ns[key] = cp
	return nil
}

func (b *MemKVBackend) Delete(pluginID, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ns := b.m[pluginID]; ns != nil {
		delete(ns, key)
	}
	return nil
}

func (b *MemKVBackend) List(pluginID, prefix string) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var keys []string
	for k := range b.m[pluginID] {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// hostStore is the Store a plugin borrows. It exposes KV and Secrets, both
// bound to the plugin id, so the plugin cannot address another namespace.
// Secrets is gated by the SAME capability as KV (CapStore, see secrets.go's
// doc comment for why) — the two differ only in which backend they delegate
// to: secretsBackend is an encryptingKVBackend over a storage extent disjoint
// from backend, so KV() and Secrets() can never see each other's values.
type hostStore struct {
	grant          Grant
	backend        KVBackend
	secretsBackend KVBackend
}

func (s *hostStore) KV() ext.KV {
	return &hostKV{pluginID: s.grant.PluginID, granted: s.grant.HasAPI(CapStore), backend: s.backend}
}

func (s *hostStore) Secrets() ext.KV {
	return &hostKV{pluginID: s.grant.PluginID, granted: s.grant.HasAPI(CapStore), backend: s.secretsBackend}
}

// Table returns a schemaless document table over the SAME backend as KV() —
// see table.go's doc comment for the isolation scheme (key-prefix, not a
// disjoint storage extent).
func (s *hostStore) Table(name string) ext.Table {
	return &hostTable{pluginID: s.grant.PluginID, name: name, granted: s.grant.HasAPI(CapStore), backend: s.backend}
}

type hostKV struct {
	pluginID string
	granted  bool
	backend  KVBackend
}

func (k *hostKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	if !k.granted {
		return nil, false, ext.ErrPermissionDenied
	}
	return k.backend.Get(k.pluginID, key)
}

func (k *hostKV) Set(_ context.Context, key string, value []byte) error {
	if !k.granted {
		return ext.ErrPermissionDenied
	}
	return k.backend.Set(k.pluginID, key, value)
}

func (k *hostKV) Delete(_ context.Context, key string) error {
	if !k.granted {
		return ext.ErrPermissionDenied
	}
	return k.backend.Delete(k.pluginID, key)
}

func (k *hostKV) List(_ context.Context, prefix string) ([]string, error) {
	if !k.granted {
		return nil, ext.ErrPermissionDenied
	}
	return k.backend.List(k.pluginID, prefix)
}
