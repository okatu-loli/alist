package ext

import (
	"encoding/json"
	"testing"
)

// TestManifestRoundTrip is a smoke test that the frozen DTOs marshal and
// unmarshal, and that the module compiles standalone (no internal/ imports).
func TestManifestRoundTrip(t *testing.T) {
	m := Manifest{
		ID:           "com.example.driver",
		Name:         map[string]string{"en": "Example"},
		Version:      "0.1.0",
		CoreAPI:      "^4.0.0",
		Kinds:        []Kind{KindStorageDriver},
		Capabilities: []Capability{CapMkdir, CapPut},
		Provenance:   Provenance{Source: "official", License: "MIT"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != m.ID || len(got.Capabilities) != 2 || got.Kinds[0] != KindStorageDriver {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLinkByteSource(t *testing.T) {
	l := Link{ByteSource: ByteSourceStream, StreamID: "s1", RangeSupport: true}
	b, _ := json.Marshal(l)
	var got Link
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ByteSource != ByteSourceStream || got.StreamID != "s1" {
		t.Fatalf("link mismatch: %+v", got)
	}
}

func TestErrCode(t *testing.T) {
	err := NewError(ErrCodeNotImplement, "nope")
	if code, ok := CodeOf(err); !ok || code != ErrCodeNotImplement {
		t.Fatalf("CodeOf = %q, %v", code, ok)
	}
	if _, ok := CodeOf(nil); ok {
		t.Fatalf("CodeOf(nil) should be false")
	}
}
