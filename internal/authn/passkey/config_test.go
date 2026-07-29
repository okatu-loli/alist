package passkey

import "testing"

func TestNew_Valid(t *testing.T) {
	w, err := New(RP{
		ID:          "example.com",
		DisplayName: "Example",
		Origins:     []string{"https://example.com", "https://app.example.com:8443"},
	})
	if err != nil {
		t.Fatalf("valid config should build: %v", err)
	}
	if w.Config.RPID != "example.com" {
		t.Fatalf("RPID mismatch: %q", w.Config.RPID)
	}
}

func TestNew_Rejects(t *testing.T) {
	cases := []struct {
		name string
		rp   RP
	}{
		{"empty id", RP{ID: "", Origins: []string{"https://example.com"}}},
		{"no origins", RP{ID: "example.com"}},
		{"id not suffix of origin", RP{ID: "example.com", Origins: []string{"https://evil.com"}}},
		{"id is sibling not parent", RP{ID: "example.com", Origins: []string{"https://notexample.com"}}},
		{"unparseable origin", RP{ID: "example.com", Origins: []string{"://://"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.rp); err == nil {
				t.Fatalf("expected rejection for %s", c.name)
			}
		})
	}
}

func TestIsRegistrableSuffix(t *testing.T) {
	cases := []struct {
		rpID, host string
		want       bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "app.example.com", true},
		{"example.com", "a.b.example.com", true},
		{"example.com", "notexample.com", false},
		{"example.com", "example.com.evil.com", false},
		{"com", "example.com", true}, // suffix-wise valid; TLD policy is out of scope here
		{"example.com", "evil.com", false},
	}
	for _, c := range cases {
		if got := isRegistrableSuffix(c.rpID, c.host); got != c.want {
			t.Errorf("isRegistrableSuffix(%q,%q)=%v want %v", c.rpID, c.host, got, c.want)
		}
	}
}

func TestDeriveRPID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://example.com", "example.com"},
		{"https://cloud.example.com:8443", "cloud.example.com"},
		{"http://localhost:5245", "localhost"},
		{"example.com", "example.com"},
		{"EXAMPLE.com:80", "example.com"},
	}
	for _, c := range cases {
		got, err := DeriveRPID(c.in)
		if err != nil {
			t.Fatalf("DeriveRPID(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("DeriveRPID(%q)=%q want %q", c.in, got, c.want)
		}
	}
	if _, err := DeriveRPID(""); err == nil {
		t.Fatal("empty siteURL should error")
	}
}
