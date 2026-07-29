package sso

import "testing"

func TestLookupPreset(t *testing.T) {
	for _, key := range []string{"casdoor", "clerk", "authing", "generic"} {
		p, ok := LookupPreset(key)
		if !ok {
			t.Fatalf("preset %q must exist", key)
		}
		if p.Key != key {
			t.Errorf("LookupPreset(%q).Key = %q", key, p.Key)
		}
		if len(p.DefaultScopes) == 0 || p.DefaultScopes[0] != "openid" {
			t.Errorf("preset %q must default to openid scope, got %v", key, p.DefaultScopes)
		}
		if p.Claims.Email == "" || p.Claims.Username == "" || p.Claims.Name == "" {
			t.Errorf("preset %q must have a full claim mapping, got %+v", key, p.Claims)
		}
		if p.DocURL == "" {
			t.Errorf("preset %q must carry a docs link", key)
		}
	}
	if _, ok := LookupPreset("nope"); ok {
		t.Fatal("unknown preset key must not resolve")
	}
}

func TestPreset_BuildIssuer(t *testing.T) {
	cases := []struct {
		key    string
		domain string
		want   string
	}{
		{"casdoor", "door.example.com", "https://door.example.com"},
		{"casdoor", "door.example.com/", "https://door.example.com"},
		{"clerk", "app.clerk.accounts.dev", "https://app.clerk.accounts.dev"},
		{"authing", "myapp.authing.cn", "https://myapp.authing.cn/oidc"},
		// generic: the operator supplies the full issuer verbatim.
		{"generic", "https://keycloak.example.com/realms/main", "https://keycloak.example.com/realms/main"},
	}
	for _, c := range cases {
		p, ok := LookupPreset(c.key)
		if !ok {
			t.Fatalf("preset %q missing", c.key)
		}
		if got := p.BuildIssuer(c.domain); got != c.want {
			t.Errorf("%s.BuildIssuer(%q) = %q, want %q", c.key, c.domain, got, c.want)
		}
	}
}

func TestPresets_ReturnsCopy(t *testing.T) {
	a := Presets()
	if len(a) == 0 {
		t.Fatal("Presets must not be empty")
	}
	a[0].Key = "mutated"
	if b := Presets(); b[0].Key == "mutated" {
		t.Fatal("Presets must return a defensive copy, not the shared backing slice")
	}
}
