package extension

import (
	"testing"

	ext "github.com/AlistGo/alist-ext"
)

// openManifest is a signable open-source manifest (has a source_url, free,
// non-proprietary), so its tier is decided purely by signature.
func openManifest() ext.Manifest {
	m := validManifest()
	m.Provenance = ext.Provenance{
		Source:    "community",
		License:   "MIT",
		SourceURL: "https://github.com/example/demo",
		Pricing:   "free",
	}
	return m
}

func TestClosedOrPaid(t *testing.T) {
	cases := []struct {
		name string
		prov ext.Provenance
		want bool
	}{
		{"open free", ext.Provenance{License: "MIT", SourceURL: "https://x", Pricing: "free"}, false},
		{"proprietary", ext.Provenance{License: "proprietary", SourceURL: "https://x"}, true},
		{"no source", ext.Provenance{License: "MIT", SourceURL: ""}, true},
		{"paid", ext.Provenance{License: "MIT", SourceURL: "https://x", Pricing: "paid"}, true},
		{"proprietary case-insensitive", ext.Provenance{License: "Proprietary", SourceURL: "https://x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := validManifest()
			m.Provenance = c.prov
			if got := ClosedOrPaid(m); got != c.want {
				t.Fatalf("ClosedOrPaid=%v want %v", got, c.want)
			}
		})
	}
}

func TestDetermineTierOfficial(t *testing.T) {
	officialPub, officialPriv := genKey(t)
	tv := NewTieredVerifier(officialPub, nil)

	dir := writePackageDir(t, openManifest())
	if err := SignDir(dir, officialPriv); err != nil {
		t.Fatal(err)
	}
	tier, src, err := tv.DetermineTier(dir, openManifest())
	if err != nil {
		t.Fatalf("DetermineTier: %v", err)
	}
	if tier != TierOfficial {
		t.Fatalf("tier=%v want official", tier)
	}
	if src != "" {
		t.Fatalf("source=%q want empty for official", src)
	}
}

func TestDetermineTierSourceVerified(t *testing.T) {
	officialPub, _ := genKey(t)
	src, srcPriv := testSource(t, "myrepo")
	tv := NewTieredVerifier(officialPub, func() []PluginSource { return []PluginSource{src} })

	dir := writePackageDir(t, openManifest())
	if err := SignDir(dir, srcPriv); err != nil {
		t.Fatal(err)
	}
	tier, name, err := tv.DetermineTier(dir, openManifest())
	if err != nil {
		t.Fatalf("DetermineTier: %v", err)
	}
	if tier != TierVerified {
		t.Fatalf("tier=%v want verified", tier)
	}
	if name != "myrepo" {
		t.Fatalf("source=%q want myrepo", name)
	}
}

func TestDetermineTierUnsignedIsThirdParty(t *testing.T) {
	officialPub, _ := genKey(t)
	src, _ := testSource(t, "myrepo")
	tv := NewTieredVerifier(officialPub, func() []PluginSource { return []PluginSource{src} })

	dir := writePackageDir(t, openManifest()) // never signed
	tier, name, err := tv.DetermineTier(dir, openManifest())
	if err != nil {
		t.Fatalf("unsigned open package should be third-party without error, got %v", err)
	}
	if tier != TierThirdParty {
		t.Fatalf("tier=%v want third-party", tier)
	}
	if name != "" {
		t.Fatalf("source=%q want empty", name)
	}
}

func TestDetermineTierForeignSignatureIsThirdPartyWithError(t *testing.T) {
	officialPub, _ := genKey(t)
	src, _ := testSource(t, "myrepo")
	_, foreignPriv := genKey(t)
	tv := NewTieredVerifier(officialPub, func() []PluginSource { return []PluginSource{src} })

	dir := writePackageDir(t, openManifest())
	if err := SignDir(dir, foreignPriv); err != nil { // signed by an unknown key
		t.Fatal(err)
	}
	tier, name, err := tv.DetermineTier(dir, openManifest())
	if tier != TierThirdParty {
		t.Fatalf("tier=%v want third-party", tier)
	}
	if name != "" {
		t.Fatalf("source=%q want empty", name)
	}
	if err == nil {
		t.Fatal("a present-but-untrusted signature should surface an error")
	}
	if _, ok := err.(*SignatureError); !ok {
		t.Fatalf("want *SignatureError, got %T", err)
	}
}

func TestDetermineTierClosedBeatsOfficialSignature(t *testing.T) {
	officialPub, officialPriv := genKey(t)
	tv := NewTieredVerifier(officialPub, nil)

	// A proprietary manifest, even if officially signed, is forced to 🔴.
	m := openManifest()
	m.Provenance.License = "proprietary"
	dir := writePackageDir(t, m)
	if err := SignDir(dir, officialPriv); err != nil {
		t.Fatal(err)
	}
	tier, name, err := tv.DetermineTier(dir, m)
	if err != nil {
		t.Fatalf("DetermineTier: %v", err)
	}
	if tier != TierThirdParty {
		t.Fatalf("closed+signed tier=%v want third-party", tier)
	}
	if name != "" {
		t.Fatalf("source=%q want empty", name)
	}
}

func TestDetermineTierClosedPaidWithBadSignatureIsHardRejected(t *testing.T) {
	officialPub, _ := genKey(t)
	src, _ := testSource(t, "myrepo")
	_, foreignPriv := genKey(t) // untrusted key
	tv := NewTieredVerifier(officialPub, func() []PluginSource { return []PluginSource{src} })

	// Proprietary manifest, signed by a key nobody trusts: the signature
	// integrity check must fire BEFORE the closed/paid short-circuit, so this
	// is a hard SignatureError rejection, not a silent 🔴.
	m := openManifest()
	m.Provenance.License = "proprietary"
	dir := writePackageDir(t, m)
	if err := SignDir(dir, foreignPriv); err != nil {
		t.Fatal(err)
	}
	tier, name, err := tv.DetermineTier(dir, m)
	if tier != TierThirdParty {
		t.Fatalf("tier=%v want third-party", tier)
	}
	if name != "" {
		t.Fatalf("source=%q want empty", name)
	}
	if err == nil {
		t.Fatal("closed/paid package with a present-but-untrusted signature must be hard-rejected, not silently 🔴")
	}
	if _, ok := err.(*SignatureError); !ok {
		t.Fatalf("want *SignatureError, got %T", err)
	}
}

func TestDetermineTierMalformedSourceKeySkipped(t *testing.T) {
	officialPub, _ := genKey(t)
	goodSrc, goodPriv := testSource(t, "goodrepo")
	badSrc := PluginSource{Name: "badrepo", IndexURL: "https://example.com/index.json", PubKey: "zz"}

	// A bad source alongside a good one: the good one still wins.
	tv := NewTieredVerifier(officialPub, func() []PluginSource { return []PluginSource{badSrc, goodSrc} })
	dir := writePackageDir(t, openManifest())
	if err := SignDir(dir, goodPriv); err != nil {
		t.Fatal(err)
	}
	tier, name, err := tv.DetermineTier(dir, openManifest())
	if err != nil {
		t.Fatalf("DetermineTier: %v", err)
	}
	if tier != TierVerified || name != "goodrepo" {
		t.Fatalf("tier=%v source=%q want (verified, goodrepo)", tier, name)
	}

	// Only the bad source: never grants trust, and an unsigned package is 🔴
	// without error.
	tvBadOnly := NewTieredVerifier(officialPub, func() []PluginSource { return []PluginSource{badSrc} })
	dir2 := writePackageDir(t, openManifest())
	tier2, name2, err2 := tvBadOnly.DetermineTier(dir2, openManifest())
	if err2 != nil {
		t.Fatalf("DetermineTier: %v", err2)
	}
	if tier2 != TierThirdParty || name2 != "" {
		t.Fatalf("tier=%v source=%q want (third-party, \"\")", tier2, name2)
	}
}

func TestSelectExecMode(t *testing.T) {
	cases := []struct {
		name      string
		tier      Tier
		hasNative bool
		wantMode  ExecMode
		wantOK    bool
	}{
		{"official native", TierOfficial, true, ExecGRPC, true},
		{"verified native", TierVerified, true, ExecGRPC, true},
		{"third-party native runs grpc too", TierThirdParty, true, ExecGRPC, true},
		{"official no binary refused", TierOfficial, false, ExecInProcess, false},
		{"third-party no binary refused", TierThirdParty, false, ExecInProcess, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mode, ok := SelectExecMode(c.tier, c.hasNative)
			if mode != c.wantMode || ok != c.wantOK {
				t.Fatalf("SelectExecMode=(%v,%v) want (%v,%v)", mode, ok, c.wantMode, c.wantOK)
			}
		})
	}
}

func TestExecModesForAllTiersGRPC(t *testing.T) {
	for _, tier := range []Tier{TierOfficial, TierVerified, TierThirdParty} {
		modes := ExecModesFor(tier)
		if len(modes) != 1 || modes[0] != ExecGRPC {
			t.Fatalf("ExecModesFor(%v)=%v want [ExecGRPC]", tier, modes)
		}
	}
}
