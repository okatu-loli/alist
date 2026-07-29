package hostcap

import (
	"context"
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

func TestHost_LoadConfig(t *testing.T) {
	called := 0
	h := New(Grant{PluginID: "p"}, Options{ConfigLoader: func() ([]byte, error) { called++; return []byte(`{"k":"v"}`), nil }})
	got, err := h.LoadConfig(context.Background())
	if err != nil || string(got) != `{"k":"v"}` {
		t.Fatalf("LoadConfig = %s, %v", got, err)
	}
	// nil loader → nil, no panic
	h2 := New(Grant{PluginID: "p"}, Options{})
	if got, err := h2.LoadConfig(context.Background()); err != nil || got != nil {
		t.Fatalf("nil-loader LoadConfig = %s, %v; want nil,nil", got, err)
	}
}

// TestHost_SaveConfig mirrors TestHost_LoadConfig in the write direction: the
// wired ConfigSaver receives exactly the bytes SaveConfig was called with, and
// a Host built without one refuses the write (silently dropping a save is
// worse than a no-op read, unlike LoadConfig's nil,nil) with ErrCodeNotImplement
// rather than pretending it persisted.
func TestHost_SaveConfig(t *testing.T) {
	var got []byte
	h := New(Grant{PluginID: "p"}, Options{ConfigSaver: func(cfg []byte) error {
		got = append([]byte(nil), cfg...)
		return nil
	}})
	want := []byte(`{"k":"v"}`)
	if err := h.SaveConfig(context.Background(), want); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("saver received %s, want %s", got, want)
	}

	// nil saver → a clear not-implemented error, not a silent no-op.
	h2 := New(Grant{PluginID: "p"}, Options{})
	err := h2.SaveConfig(context.Background(), want)
	if err == nil {
		t.Fatal("nil-saver SaveConfig: expected an error, got nil")
	}
	if code, ok := ext.CodeOf(err); !ok || code != ext.ErrCodeNotImplement {
		t.Fatalf("nil-saver SaveConfig code = %v,%v; want %v,true", code, ok, ext.ErrCodeNotImplement)
	}
}

// TestHost_Capabilities returns the granted Host-API names for a Host built
// with a known grant, and an empty slice for one with an empty grant.
func TestHost_Capabilities(t *testing.T) {
	h := New(Grant{
		PluginID: "p",
		HostAPIs: []GrantItem{{Name: CapFS}, {Name: CapStore, Scope: "kv"}},
	}, Options{})
	got, err := h.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	want := []string{CapFS, CapStore}
	if len(got) != len(want) {
		t.Fatalf("Capabilities = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("Capabilities = %v, want %v", got, want)
		}
	}

	h2 := New(Grant{PluginID: "p"}, Options{})
	got2, err := h2.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities (empty grant): %v", err)
	}
	if len(got2) != 0 {
		t.Fatalf("Capabilities (empty grant) = %v, want empty", got2)
	}
}
