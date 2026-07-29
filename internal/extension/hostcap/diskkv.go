package hostcap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DiskKVBackend is a persistent KVBackend: each plugin's namespace is one
// <baseDir>/<pluginID>/kv.json file, lazily loaded and written through
// atomically (temp+rename). Same interface as MemKVBackend, so hostStore's grant
// gating and namespace isolation are unchanged. Because the file lives inside the
// plugin's own dir, Manager.Uninstall (RemoveAll(<dir>/<id>)) cleans it for free.
type DiskKVBackend struct {
	baseDir string
	mu      sync.Mutex
	cache   map[string]map[string][]byte // pluginID → loaded namespace (nil entry until first touch)
}

var _ KVBackend = (*DiskKVBackend)(nil)

// NewDiskKVBackend builds a persistent KV backend rooted at baseDir (the plugin
// root dir; each plugin's data lands at <baseDir>/<id>/kv.json).
func NewDiskKVBackend(baseDir string) *DiskKVBackend {
	return &DiskKVBackend{baseDir: baseDir, cache: map[string]map[string][]byte{}}
}

func (b *DiskKVBackend) path(pluginID string) string {
	return filepath.Join(b.baseDir, pluginID, "kv.json")
}

// ns lazily loads pluginID's namespace on first access. Missing file = empty
// namespace (not an error); corrupt file = empty (fail-safe). Caller holds b.mu.
func (b *DiskKVBackend) ns(pluginID string) map[string][]byte {
	if m, ok := b.cache[pluginID]; ok {
		return m
	}
	m := map[string][]byte{}
	if data, err := os.ReadFile(b.path(pluginID)); err == nil {
		if json.Unmarshal(data, &m) != nil {
			m = map[string][]byte{} // corrupt kv.json → fail-safe empty
		}
	}
	b.cache[pluginID] = m
	return m
}

// persist atomically writes pluginID's namespace. Caller holds b.mu.
func (b *DiskKVBackend) persist(pluginID string) error {
	if err := os.MkdirAll(filepath.Join(b.baseDir, pluginID), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(b.cache[pluginID])
	if err != nil {
		return err
	}
	tmp := b.path(pluginID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.path(pluginID))
}

func (b *DiskKVBackend) Get(pluginID, key string) ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.ns(pluginID)[key]
	if !ok {
		return nil, false, nil
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, true, nil
}

func (b *DiskKVBackend) Set(pluginID, key string, value []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(value))
	copy(cp, value)
	b.ns(pluginID)[key] = cp
	return b.persist(pluginID)
}

func (b *DiskKVBackend) Delete(pluginID, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.ns(pluginID)
	if _, ok := m[key]; !ok {
		return nil
	}
	delete(m, key)
	return b.persist(pluginID)
}

func (b *DiskKVBackend) List(pluginID, prefix string) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for k := range b.ns(pluginID) {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
