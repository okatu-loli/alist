package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// This file holds the TOKEN primitives for TOKEN-AUTHENTICATED reverse pairing
// (design subproject F, wave F1). It REPLACES the human-compared 5-emoji SAS
// (DeriveReverseSAS) as the anti-MITM anchor of reverse pairing: instead of a
// human eyeballing two short renderings, a high-entropy one-time token T is
// generated on the server, copied out of band to the agent's pairing page, and
// both sides use T as an HMAC key to prove — cryptographically — that they hold
// the SAME secret over the SAME handshake transcript (see pairing_handshake.go
// reversePairTag). Token beats "compare emoji" on the "operator手滑" axis (it is
// pass-or-fail crypto, not an eyeballed match) but it changes the assumption: T
// is a SECRET that must travel a CONFIDENTIAL out-of-band channel (T leaks =
// full MITM), whereas the SAS only ever needed an INTEGRITY-protected compare
// channel. That trade-off is deliberate and documented in the design spec.
//
// 🔒 THE LOAD-BEARING INVARIANT (design spec Critical #2): the token's entropy
// is ENFORCED IN CODE. There is NO parameter to lower it and it is NEVER
// operator-chosen. A low-entropy T captured on the wire (tags are HMAC(T, …)
// over public values) could be brute-forced offline → full MITM, a catastrophic
// outcome. So the byte length is a fixed constant here and the only public
// generator draws exactly that many bytes from crypto/rand.

const (
	// reversePairTokenBytes is the RAW entropy of a reverse-pairing token: 32
	// bytes = 256 bits from crypto/rand. FIXED — there is deliberately no knob to
	// lower it (design spec Critical #2). 256 bits is far beyond any offline
	// brute-force even though the token is only an HMAC key over public transcript
	// values; the whole scheme's safety rests on this being un-guessable.
	reversePairTokenBytes = 32
)

// reversePairTokenEncoding is the base32 alphabet tokens are displayed in
// (RFC-4648 upper-case A–Z2–7), WITHOUT padding: 32 raw bytes encode to 52
// characters. Padding is dropped on output and re-tolerated on input by the
// canonicalizer, so a pasted token with or without cosmetic '=' both decode.
var reversePairTokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateReversePairToken mints a fresh one-time reverse-pairing token: exactly
// reversePairTokenBytes (32) bytes from crypto/rand, base32-encoded for display.
// The entropy is FIXED IN CODE — there is intentionally no argument to shorten
// it and it is never operator-supplied (design spec Critical #2). The returned
// string is what the server shows the operator to copy to the agent's pairing
// page; CanonicalizeReversePairToken turns a pasted copy back into the 32 raw
// bytes both sides key their HMAC tags with.
func GenerateReversePairToken() (string, error) {
	raw := make([]byte, reversePairTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agentpair: read random token: %w", err)
	}
	return reversePairTokenEncoding.EncodeToString(raw), nil
}

// CanonicalizeReversePairToken normalizes a PASTED base32 token and returns the
// 32 raw bytes it decodes to. It is lenient about the cosmetic differences a
// correct copy can pick up (design spec Minor #9): it upper-cases (base32 is
// case-insensitive by alphabet), strips ALL whitespace (spaces, tabs, newlines
// a copy/paste may introduce), and drops '=' padding — so a token that was
// generated here and copied correctly always verifies regardless of case/
// padding/whitespace noise. It then decodes and REQUIRES exactly
// reversePairTokenBytes (32) bytes: anything that does not decode to the fixed
// length is rejected, so a truncated/corrupted paste can never yield a short
// (weakened) HMAC key.
func CanonicalizeReversePairToken(pasted string) ([]byte, error) {
	var b strings.Builder
	b.Grow(len(pasted))
	for _, r := range pasted {
		if unicode.IsSpace(r) || r == '=' {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	raw, err := reversePairTokenEncoding.DecodeString(b.String())
	if err != nil {
		return nil, errors.New("agentpair: reverse-pairing token is not valid base32")
	}
	if len(raw) != reversePairTokenBytes {
		return nil, fmt.Errorf("agentpair: reverse-pairing token must decode to %d bytes (got %d)", reversePairTokenBytes, len(raw))
	}
	return raw, nil
}

// ValidatePairingToken enforces the token invariant at the handshake boundary:
// the raw token both sides key their tags with MUST be exactly
// reversePairTokenBytes (32). A nil/short token can never key a valid tag, so a
// caller — the handshake (pairing_handshake.go) itself, or an external dialer
// like internal/agentsrv/pairing_dial.go — fails closed BEFORE any exchange
// rather than run a doomed handshake. It lives here, not in
// pairing_handshake.go, because it is fundamentally a token-shape check (the
// handshake only ever calls it, never redefines it).
func ValidatePairingToken(tokenRaw []byte) error {
	if len(tokenRaw) != reversePairTokenBytes {
		return fmt.Errorf("agentpair: reverse-pairing token must be %d raw bytes (got %d)", reversePairTokenBytes, len(tokenRaw))
	}
	return nil
}

// reversePairFingerprintBytes is how many leading bytes of SHA-256(serverPub)
// the DISPLAY fingerprint renders (8 bytes = 64 bits, eight colon-separated hex
// octets). Display only — see ReversePairFingerprint.
const reversePairFingerprintBytes = 8

// ReversePairFingerprint renders a short, human-glanceable fingerprint of a
// public key: the first reversePairFingerprintBytes of SHA-256(pub) as
// colon-separated lower-case hex (e.g. "3a:f2:9c:…"). It is NON-LOAD-BEARING
// (design spec「取舍与指纹」): the token tags are the sole anti-MITM anchor, and
// the fingerprint is offered purely so an operator can optionally eyeball the
// server key on both ends for peace of mind. Both sides can compute the SERVER's
// fingerprint (the agent learns serverPub from M2; the server holds its own), so
// it renders identically on both — but nothing about pairing security depends on
// a human comparing it. Returns "" for a malformed/empty key so a display path
// never panics.
func ReversePairFingerprint(pub []byte) string {
	if len(pub) == 0 {
		return ""
	}
	sum := sha256.Sum256(pub)
	n := reversePairFingerprintBytes
	if n > len(sum) {
		n = len(sum)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("%02x", sum[i])
	}
	return strings.Join(parts, ":")
}
