package extension

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A plugin package is a directory containing plugin.json, the binary(ies), a
// checksums file, and a signature file. The checksums file pins every other
// file's sha256; the signature is an ed25519 signature over the raw bytes of the
// checksums file. Verifying both means: no file was altered (checksum), and the
// checksum list itself was produced by a holder of the trust-root private key
// (signature). This is the "sign the sha256 checksum manifest of all files"
// scheme from the 9.3 brief.
const (
	// ChecksumsFile lists "<sha256hex>␠␠<relpath>" for every packaged file
	// except itself and SignatureFile, sorted by relpath.
	ChecksumsFile = "checksums.txt"
	// SignatureFile holds the base64 ed25519 signature over ChecksumsFile's bytes.
	SignatureFile = "signature"
)

// ComputeChecksums walks dir and returns the canonical checksums.txt content:
// one "<sha256hex>  <relpath>\n" line per file (excluding ChecksumsFile and
// SignatureFile), sorted by relpath so the output is deterministic.
func ComputeChecksums(dir string) (string, error) {
	type line struct{ rel, sum string }
	var lines []line
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ChecksumsFile || rel == SignatureFile {
			return nil
		}
		sum, err := sha256File(path)
		if err != nil {
			return err
		}
		lines = append(lines, line{rel: rel, sum: sum})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].rel < lines[j].rel })
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s  %s\n", l.sum, l.rel)
	}
	return b.String(), nil
}

// hasSignatureArtifacts reports whether dir carries the signing files at all, so
// a caller can tell "package presented a signature (verify it)" from "package is
// plainly unsigned" (a third-party package that never claimed to be signed).
func hasSignatureArtifacts(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, SignatureFile)); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ChecksumsFile)); err != nil {
		return false
	}
	return true
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SignDir writes ChecksumsFile and SignatureFile into dir, signing the checksum
// manifest with priv. Used by the official signer (cmd/plugin_sign.go).
func SignDir(dir string, priv ed25519.PrivateKey) error {
	content, err := ComputeChecksums(dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ChecksumsFile), []byte(content), 0o644); err != nil {
		return err
	}
	sig := ed25519.Sign(priv, []byte(content))
	enc := base64.StdEncoding.EncodeToString(sig)
	return os.WriteFile(filepath.Join(dir, SignatureFile), []byte(enc+"\n"), 0o644)
}

// Verifier holds the trust roots a package may verify against. Today the
// official verifier carries exactly one key; Step 11 trust tiers will add roots
// (verified-community / third-party) and map them to execution modes — the
// multi-key slice is that seam.
type Verifier struct {
	keys []ed25519.PublicKey
}

// NewVerifier builds a verifier over the given trust roots (used by tests to
// supply their own key; production uses OfficialVerifier).
func NewVerifier(keys ...ed25519.PublicKey) *Verifier {
	return &Verifier{keys: keys}
}

// SignatureError marks a package that failed signature/checksum verification, so
// callers can distinguish "untrusted" from other install failures and reject
// with a clear reason (9.3 gating).
type SignatureError struct{ Reason string }

func (e *SignatureError) Error() string { return "plugin signature invalid: " + e.Reason }

// Verify checks that dir's files match ChecksumsFile and that the signature over
// ChecksumsFile validates against one of the trust roots. A verification failure
// returns *SignatureError.
func (v *Verifier) Verify(dir string) error {
	recorded, err := os.ReadFile(filepath.Join(dir, ChecksumsFile))
	if err != nil {
		return &SignatureError{Reason: "missing checksums: " + err.Error()}
	}
	sigRaw, err := os.ReadFile(filepath.Join(dir, SignatureFile))
	if err != nil {
		return &SignatureError{Reason: "missing signature: " + err.Error()}
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
	if err != nil {
		return &SignatureError{Reason: "malformed signature encoding"}
	}
	// Signature must validate against a trust root over the recorded manifest.
	trusted := false
	for _, k := range v.keys {
		if ed25519.Verify(k, recorded, sig) {
			trusted = true
			break
		}
	}
	if !trusted {
		return &SignatureError{Reason: "not signed by a trusted key"}
	}
	// The recorded manifest must match the actual files (both directions: no
	// altered file, no unlisted extra file).
	actual, err := ComputeChecksums(dir)
	if err != nil {
		return err
	}
	if actual != string(recorded) {
		return &SignatureError{Reason: "checksum mismatch (package contents altered)"}
	}
	return nil
}
