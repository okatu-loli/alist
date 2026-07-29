package sso

import (
	"testing"
)

func sampleConfig(idp *mockIdP) ProviderConfig {
	return ProviderConfig{
		Key:      "casdoor",
		Name:     "Casdoor",
		Preset:   "casdoor",
		Issuer:   idp.issuer,
		ClientID: idp.clientID,
		Enabled:  true,
	}
}

func TestMethodIDRoundTrip(t *testing.T) {
	if got := MethodID("casdoor"); got != "sso:casdoor" {
		t.Fatalf("MethodID = %q, want sso:casdoor", got)
	}
	key, ok := KeyFromMethodID("sso:casdoor")
	if !ok || key != "casdoor" {
		t.Fatalf("KeyFromMethodID = %q,%v, want casdoor,true", key, ok)
	}
	if _, ok := KeyFromMethodID("builtin.passkey"); ok {
		t.Fatal("a non-sso method id must not be recognized as an sso key")
	}
	if _, ok := KeyFromMethodID("sso:"); ok {
		t.Fatal("an empty sso key must be rejected")
	}
}

func TestProviderConfigValidate(t *testing.T) {
	base := ProviderConfig{Key: "ok", Name: "OK", Issuer: "https://i", ClientID: "c"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	bad := []ProviderConfig{
		{Key: "", Name: "n", Issuer: "i", ClientID: "c"},                   // empty key
		{Key: "has:colon", Name: "n", Issuer: "i", ClientID: "c"},          // ':' would corrupt the link key
		{Key: "has space", Name: "n", Issuer: "i", ClientID: "c"},          // whitespace
		{Key: "ok", Name: "", Issuer: "i", ClientID: "c"},                  // no name
		{Key: "ok", Name: "n", Issuer: "", ClientID: "c"},                  // no issuer
		{Key: "ok", Name: "n", Issuer: "i", ClientID: ""},                  // no client id
		{Key: "ok", Name: "n", Issuer: "i", ClientID: "c", Preset: "nope"}, // unknown preset
	}
	for i, pc := range bad {
		if err := pc.Validate(); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, pc)
		}
	}
}

func TestToOIDCConfigDefaultsClaims(t *testing.T) {
	pc := ProviderConfig{Key: "k", Issuer: "https://i", ClientID: "c"}
	cfg := pc.toOIDCConfig("https://app/cb")
	if cfg.RedirectURL != "https://app/cb" {
		t.Fatalf("redirect url not bound: %q", cfg.RedirectURL)
	}
	// Explicit claim overrides are carried; empties are filled by the engine's
	// withDefaults at construction, not here.
	pc.ClaimEmail = "mail"
	if got := pc.toOIDCConfig("x").Claims.Email; got != "mail" {
		t.Fatalf("claim override lost: %q", got)
	}
}

func TestBuildProviderCachesAndRebuilds(t *testing.T) {
	SetHTTPClient(noProxyClient())
	t.Cleanup(func() { SetHTTPClient(nil) })
	idp := newMockIdP(t)
	pc := sampleConfig(idp)

	p1, err := BuildProvider(pc, "https://app/cb")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	p2, err := BuildProvider(pc, "https://app/cb")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatal("same config+redirect must reuse the cached provider instance")
	}
	// A changed redirect URL (or edited config) must rebuild.
	p3, err := BuildProvider(pc, "https://other/cb")
	if err != nil {
		t.Fatal(err)
	}
	if p3 == p1 {
		t.Fatal("a changed redirect URL must produce a fresh provider")
	}
	// Editing a field rebuilds too.
	pc.ClientSecret = "changed"
	p4, _ := BuildProvider(pc, "https://app/cb")
	if p4 == p1 {
		t.Fatal("an edited config must produce a fresh provider")
	}

	InvalidateProvider(pc.Key)
	p5, _ := BuildProvider(pc, "https://app/cb")
	if p5 == p4 {
		t.Fatal("InvalidateProvider must force a rebuild")
	}
}

func TestBuildProviderRejectsInvalidEngineConfig(t *testing.T) {
	SetHTTPClient(noProxyClient())
	t.Cleanup(func() { SetHTTPClient(nil) })
	// Missing client id trips NewOIDCProvider's validation.
	pc := ProviderConfig{Key: "bad", Name: "Bad", Issuer: "https://i"}
	if _, err := BuildProvider(pc, "https://app/cb"); err == nil {
		t.Fatal("expected BuildProvider to surface the engine's config validation error")
	}
}
