package extension

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func testSource(t *testing.T, name string) (PluginSource, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return PluginSource{Name: name, IndexURL: "https://example.com/index.json",
		PubKey: hex.EncodeToString(pub)}, priv
}

func TestPluginSourceValidate(t *testing.T) {
	s, _ := testSource(t, "myrepo")
	if err := s.Validate(); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	cases := []PluginSource{
		{Name: "", IndexURL: s.IndexURL, PubKey: s.PubKey},         // empty name
		{Name: "official", IndexURL: s.IndexURL, PubKey: s.PubKey}, // reserved
		{Name: "x", IndexURL: "ftp://nope", PubKey: s.PubKey},      // bad scheme
		{Name: "x", IndexURL: s.IndexURL, PubKey: "zz"},            // bad key
		{Name: "my repo", IndexURL: s.IndexURL, PubKey: s.PubKey},  // embedded space
		{Name: `my"repo`, IndexURL: s.IndexURL, PubKey: s.PubKey},  // embedded quote
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d: invalid source accepted", i)
		}
	}
}

func TestParseSources(t *testing.T) {
	if ss, err := ParseSources(""); err != nil || len(ss) != 0 {
		t.Fatalf("empty: %v %v", ss, err)
	}
	ss, err := ParseSources(`[{"name":"a","index_url":"https://a.example/i.json","pubkey":"00"}]`)
	if err != nil || len(ss) != 1 || ss[0].Name != "a" {
		t.Fatalf("parse: %v %v", ss, err)
	}
	if _, err := ParseSources(`{not json`); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}
