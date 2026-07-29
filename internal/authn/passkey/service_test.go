package passkey

import (
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestSplitOrigins(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"https://a.example.com", []string{"https://a.example.com"}},
		{"https://a.example.com, https://b.example.com", []string{"https://a.example.com", "https://b.example.com"}},
		{"https://a.example.com\nhttps://b.example.com\n", []string{"https://a.example.com", "https://b.example.com"}},
		{" https://a.example.com ,, https://b.example.com ", []string{"https://a.example.com", "https://b.example.com"}},
	}
	for _, c := range cases {
		got := splitOrigins(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitOrigins(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitOrigins(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestOriginOf(t *testing.T) {
	ok := map[string]string{
		"https://example.com":                            "https://example.com",
		"https://example.com/":                           "https://example.com",
		"https://cloud.example.com:8443/api/v4/callback": "https://cloud.example.com:8443",
		"http://localhost:5244/x":                        "http://localhost:5244",
	}
	for in, want := range ok {
		got, err := originOf(in)
		if err != nil {
			t.Fatalf("originOf(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("originOf(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "example.com", "/relative/path"} {
		if _, err := originOf(bad); err == nil {
			t.Fatalf("originOf(%q) should error (no scheme/host)", bad)
		}
	}
}

// TestCeremonyStore_SingleUseAndExpiry pins the anti-replay spine: a stored
// ceremony is redeemable exactly once, and only before its TTL elapses. The RP
// stored alongside the session round-trips so finish can rebuild the same config.
func TestCeremonyStore_SingleUseAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newCeremonyStore(func() time.Time { return now })

	rp := RP{ID: "example.com", DisplayName: "Example", Origins: []string{"https://example.com"}}
	sess := &webauthn.SessionData{Challenge: "abc"}
	s.put("c1", sess, rp, time.Minute)

	got, ok := s.take("c1")
	if !ok {
		t.Fatal("first take must find the ceremony")
	}
	if got.rp.ID != "example.com" || got.session.Challenge != "abc" {
		t.Fatalf("stored ceremony did not round-trip: %+v", got)
	}
	// Single use: a second take finds nothing.
	if _, ok := s.take("c1"); ok {
		t.Fatal("a ceremony must not be redeemable twice")
	}

	// Expiry: put then advance past the TTL — take evicts and reports absent.
	s.put("c2", sess, rp, time.Minute)
	now = now.Add(2 * time.Minute)
	if _, ok := s.take("c2"); ok {
		t.Fatal("an expired ceremony must not be redeemable")
	}
}
