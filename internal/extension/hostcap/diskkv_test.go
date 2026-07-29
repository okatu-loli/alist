package hostcap

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDiskKV_RoundTripPersistIsolation(t *testing.T) {
	dir := t.TempDir()
	b := NewDiskKVBackend(dir)
	// get before set → not ok
	if _, ok, err := b.Get("p1", "k"); err != nil || ok {
		t.Fatalf("get before set: ok=%v err=%v", ok, err)
	}
	if err := b.Set("p1", "k", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := b.Get("p1", "k"); !ok || string(v) != "v1" {
		t.Fatalf("get after set = %q ok=%v", v, ok)
	}
	// namespace isolation: p2 can't see p1's key
	if _, ok, _ := b.Get("p2", "k"); ok {
		t.Fatal("p2 must not see p1's key")
	}
	// persist across "restart": a fresh backend over the same dir reads it back
	b2 := NewDiskKVBackend(dir)
	if v, ok, _ := b2.Get("p1", "k"); !ok || string(v) != "v1" {
		t.Fatalf("after reload = %q ok=%v", v, ok)
	}
	// file lives at <dir>/p1/kv.json, no leftover .tmp
	if _, err := os.Stat(filepath.Join(dir, "p1", "kv.json")); err != nil {
		t.Fatalf("kv.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p1", "kv.json.tmp")); !os.IsNotExist(err) {
		t.Fatal(".tmp must not remain after atomic write")
	}
	// List prefix
	_ = b.Set("p1", "pre:a", []byte("1"))
	_ = b.Set("p1", "pre:b", []byte("2"))
	ks, _ := b.List("p1", "pre:")
	if len(ks) != 2 {
		t.Fatalf("list prefix = %v, want 2", ks)
	}
	// Delete
	_ = b.Delete("p1", "k")
	if _, ok, _ := b.Get("p1", "k"); ok {
		t.Fatal("key present after delete")
	}
}

func TestDiskKV_CorruptFileFailsSafe(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p", "kv.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := NewDiskKVBackend(dir)
	if _, ok, err := b.Get("p", "k"); err != nil || ok {
		t.Fatalf("corrupt file must read as empty: ok=%v err=%v", ok, err)
	}
	// still writable after corruption
	if err := b.Set("p", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
}

func TestDiskKV_ConcurrentRace(t *testing.T) {
	dir := t.TempDir()
	b := NewDiskKVBackend(dir)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid := "p" + string(rune('a'+i%3))
			_ = b.Set(pid, "k", []byte{byte(i)})
			_, _, _ = b.Get(pid, "k")
			_, _ = b.List(pid, "")
		}(i)
	}
	wg.Wait()
}
