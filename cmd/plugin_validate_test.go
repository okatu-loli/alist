package cmd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/extension"
)

// validPluginManifest is a well-formed, open-source, free manifest with one
// Additional config field (so expectedKeys is non-empty) — the common shape
// `alist plugin validate` should accept cleanly.
func validPluginManifest() ext.Manifest {
	return ext.Manifest{
		ID:      "com.example.demo",
		Name:    map[string]string{"en": "Demo"},
		Version: "1.0.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindStorageDriver},
		Provenance: ext.Provenance{
			Source:    "community",
			License:   "MIT",
			SourceURL: "https://github.com/example/demo",
			Pricing:   "free",
		},
		Info: &ext.Info{Additional: []ext.Item{{Name: "client_id", Help: "The client id"}}},
	}
}

func writeExtManifest(t *testing.T, dir string, m ext.Manifest) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeDummyBinary(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeI18nFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "i18n", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFullEnLocale writes an i18n/en.json carrying every key expectedKeys(m)
// derives from m, so an i18n check against it is clean.
func writeFullEnLocale(t *testing.T, dir string, m ext.Manifest) {
	t.Helper()
	keys := expectedKeys(m)
	en := make(map[string]string, len(keys))
	for _, k := range keys {
		en[k] = "x"
	}
	b, err := json.Marshal(en)
	if err != nil {
		t.Fatal(err)
	}
	writeI18nFile(t, dir, "en.json", string(b))
}

func TestPluginValidateCleanPackage(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if !ok {
		t.Fatalf("want ok=true for a clean package, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "manifest: ok") {
		t.Errorf("report missing %q:\n%s", "manifest: ok", out.String())
	}
}

func TestPluginValidateBadManifest(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	m.ID = "bad id!" // violates pluginIDRe
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if ok {
		t.Fatalf("want ok=false for an invalid manifest id, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "manifest") {
		t.Errorf("report missing %q:\n%s", "manifest", out.String())
	}
}

func TestPluginValidateMissingBinary(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeFullEnLocale(t, dir, m)
	// deliberately no binary written

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if ok {
		t.Fatalf("want ok=false when the package ships no binary, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "binary") {
		t.Errorf("report missing %q:\n%s", "binary", out.String())
	}
}

func TestPluginValidateI18nMissingKey(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest() // declares field.client_id.{label,help}
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	// en.json carries .help but not .label
	writeI18nFile(t, dir, "en.json", `{"field.client_id.help":"The client id"}`)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if ok {
		t.Fatalf("want ok=false for a missing i18n key, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "field.client_id.label") {
		t.Errorf("report missing the missing key name %q:\n%s", "field.client_id.label", out.String())
	}
}

func TestPluginValidateMalformedLocale(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)
	// zh.json is not valid JSON at all — production LoadPluginLocales would
	// silently skip it; validate must surface it.
	writeI18nFile(t, dir, "zh.json", `{not valid json`)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if ok {
		t.Fatalf("want ok=false for a malformed bundled locale file, report:\n%s", out.String())
	}
}

func TestPluginValidateBrokenSignature(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}
	// tamper: alter a packaged file's content after signing, so the recorded
	// checksums manifest no longer matches what's on disk.
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("tampered content"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	ok, rerr := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if rerr != nil {
		t.Fatalf("runPluginValidate: %v", rerr)
	}
	if ok {
		t.Fatalf("want ok=false for a broken/untrusted signature, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "signature") {
		t.Errorf("report missing %q:\n%s", "signature", out.String())
	}
}

func TestPluginValidatePubkeyRecognizesSelfSignedPackage(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}

	var out bytes.Buffer
	ok, rerr := runPluginValidate(pluginValidateOptions{
		Dir:     dir,
		PubKeys: []string{hex.EncodeToString(pub)},
	}, &out)
	if rerr != nil {
		t.Fatalf("runPluginValidate: %v", rerr)
	}
	if !ok {
		t.Fatalf("a package signed with a key supplied via --pubkey should verify, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "verified") {
		t.Errorf("report missing %q:\n%s", "verified", out.String())
	}
}

func TestPluginValidateUnrecognizedSignatureHintsPubkeyFlag(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}

	// Same self-signed package as the pubkey test above, but --pubkey is NOT
	// passed: the signature is present and clean, yet unrecognized by this
	// offline check — this must stay a hard error (never silently downgrade
	// to third-party), and the message must point the author at --pubkey.
	var out bytes.Buffer
	ok, rerr := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if rerr != nil {
		t.Fatalf("runPluginValidate: %v", rerr)
	}
	if ok {
		t.Fatalf("want ok=false for an unrecognized (but not necessarily tampered) signature, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "--pubkey") {
		t.Errorf("report missing the %q hint:\n%s", "--pubkey", out.String())
	}
}

func TestPluginValidatePubkeyTamperedStillHardFails(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.SignDir(dir, priv); err != nil {
		t.Fatalf("SignDir: %v", err)
	}
	// Tamper AFTER signing: even though --pubkey supplies the right key, the
	// on-disk contents no longer match the recorded checksums, so this must
	// still hard-fail rather than pass just because the key is recognized.
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("tampered after pubkey-trusted sign"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	ok, rerr := runPluginValidate(pluginValidateOptions{
		Dir:     dir,
		PubKeys: []string{hex.EncodeToString(pub)},
	}, &out)
	if rerr != nil {
		t.Fatalf("runPluginValidate: %v", rerr)
	}
	if ok {
		t.Fatalf("a tampered package must hard-fail even when --pubkey recognizes the signing key, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "signature") {
		t.Errorf("report missing %q:\n%s", "signature", out.String())
	}
}

// TestPluginValidateBadPubkeyHex verifies --pubkey values that don't decode
// as a well-formed ed25519 public key are reported as an explicit error
// rather than silently doing nothing (task M: offlineTieredVerifier's
// downstream DetermineTier would otherwise skip a malformed source key with
// no indication the flag itself was the problem).
func TestPluginValidateBadPubkeyHex(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{
		Dir:     dir,
		PubKeys: []string{"not-valid-hex-zzz"},
	}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if ok {
		t.Fatalf("want ok=false for a malformed --pubkey value, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pubkey") || !strings.Contains(out.String(), "ed25519") {
		t.Errorf("report missing the pubkey/ed25519 error:\n%s", out.String())
	}
}

// TestPluginValidateBadPubkeyWrongLength verifies a pubkey that decodes as
// valid hex but the wrong byte length (not ed25519.PublicKeySize) is also
// rejected, not just outright-invalid hex.
func TestPluginValidateBadPubkeyWrongLength(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{
		Dir:     dir,
		PubKeys: []string{"deadbeef"}, // valid hex, wrong length
	}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if ok {
		t.Fatalf("want ok=false for a wrong-length --pubkey value, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pubkey") {
		t.Errorf("report missing the pubkey error:\n%s", out.String())
	}
}

// TestPluginValidateClosedOrPaidThirdParty verifies an unsigned
// closed-source/paid (extension.ClosedOrPaid) package gets the distinct
// "third-party(闭源/付费包...)" explanation rather than being reported
// identically to an ordinary unsigned open-source package (task M4).
func TestPluginValidateClosedOrPaidThirdParty(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	m.Provenance.License = "proprietary"
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if !ok {
		t.Fatalf("an unsigned closed/paid package is still a legitimate (if forced) third-party tier, want ok=true, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "third-party(闭源/付费包") {
		t.Errorf("report missing the closed/paid explanation:\n%s", out.String())
	}
}

func TestPluginValidateUnsignedIsWarning(t *testing.T) {
	dir := t.TempDir()
	m := validPluginManifest()
	writeExtManifest(t, dir, m)
	writeDummyBinary(t, dir)
	writeFullEnLocale(t, dir, m)

	var out bytes.Buffer
	ok, err := runPluginValidate(pluginValidateOptions{Dir: dir}, &out)
	if err != nil {
		t.Fatalf("runPluginValidate: %v", err)
	}
	if !ok {
		t.Fatalf("an unsigned third-party package is a legitimate tier, want ok=true, report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "third-party") {
		t.Errorf("report missing %q:\n%s", "third-party", out.String())
	}

	var strict bytes.Buffer
	ok2, err2 := runPluginValidate(pluginValidateOptions{Dir: dir, Strict: true}, &strict)
	if err2 != nil {
		t.Fatalf("runPluginValidate --strict: %v", err2)
	}
	if ok2 {
		t.Fatalf("--strict must reject an unsigned package, report:\n%s", strict.String())
	}
}
