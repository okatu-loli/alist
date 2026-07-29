package channel

// This file is the AEAD layer that makes the reverse-connection frame stream
// confidential, integrity-protected, and forward-secret over a plain ws://
// transport (design R2.5). R2 built a mutual-auth handshake (two ed25519
// signatures) that proves WHO the peers are, but derived NO key and did not
// mandate TLS — so on plain ws:// a network MITM who merely RELAYS the
// (unforgeable) handshake could then read, inject, or tamper with every
// post-handshake frame. R2.5 closes that hole: the handshake additionally
// carries an AUTHENTICATED ephemeral X25519 public key from each side (each
// signed under that side's long-term ed25519 key, so a MITM cannot substitute
// one without breaking a signature); both sides run X25519 ECDH over those
// ephemerals to a shared secret; HKDF-SHA256 (salted with both handshake
// nonces) stretches it into a 32-byte session key; and every frame is then
// sealed with ChaCha20-Poly1305. A relay that flips a single byte of any sealed
// frame fails the AEAD tag and the channel is torn down.
//
// FORWARD SECRECY: the X25519 ephemerals are generated fresh per handshake and
// never persisted, so a later compromise of the long-term ed25519 identity keys
// cannot decrypt a previously-recorded session.

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// hkdfInfo domain-separates this AEAD session key from any other key that
	// might be derived from the same ECDH secret. The trailing version tag means
	// a future frame-crypto scheme derives an INDEPENDENT key from an identical
	// handshake secret rather than colliding with v1.
	hkdfInfo = "agentchan-aead-v1"

	// aeadKeyBytes / aeadNonceBytes are ChaCha20-Poly1305's fixed sizes (256-bit
	// key, 96-bit nonce). Named here so the nonce scheme below is self-documenting.
	aeadKeyBytes   = chacha20poly1305.KeySize   // 32
	aeadNonceBytes = chacha20poly1305.NonceSize // 12
)

// Direction tags occupy the FIRST byte of every 12-byte AEAD nonce. Because the
// two directions carry DIFFERENT tag bytes, their nonce spaces are provably
// disjoint for ALL counter values: a server→agent nonce always begins 0x00, an
// agent→server nonce always begins 0x01. ChaCha20-Poly1305 is catastrophically
// broken if a nonce ever repeats under one key; splitting the space by direction
// means the two counters — which each independently start at 0 — can never
// collide with one another, and the monotonic counter guarantees uniqueness
// within a direction. Together: no nonce is EVER reused under the session key.
const (
	dirServerToAgent byte = 0x00
	dirAgentToServer byte = 0x01
)

// generateEphemeral mints a FRESH X25519 ephemeral key pair for exactly ONE
// handshake. Freshness-per-handshake is what gives the channel forward secrecy:
// the private scalar exists only long enough to compute the shared secret and is
// never written anywhere, so compromising the long-term ed25519 keys later
// cannot recover a past session key.
func generateEphemeral() (*ecdh.PrivateKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("agentchan: generate X25519 ephemeral: %w", err)
	}
	return priv, nil
}

// ephemeralPublicB64 is the wire encoding (base64 std of the 32-byte X25519
// public key) placed in ServerHello.EphS / AgentHello.EphA. The PUBLIC key is
// not secret; it is authenticated by the side's ed25519 signature.
func ephemeralPublicB64(priv *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
}

// parseEphemeral decodes and validates a peer's base64 ephemeral PUBLIC key as a
// well-formed X25519 point. crypto/ecdh.NewPublicKey enforces the 32-byte
// length here; the stronger all-zero / low-order rejection happens inside ECDH
// (deriveSessionKey), which fails closed on a small-subgroup result.
func parseEphemeral(b64 string) (*ecdh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("agentchan: peer ephemeral key is malformed base64")
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("agentchan: peer ephemeral key is not a valid X25519 point: %w", err)
	}
	return pub, nil
}

// deriveSessionKey computes the X25519 shared secret from our ephemeral private
// key and the peer's ephemeral public key, then stretches it into the 32-byte
// AEAD session key with HKDF-SHA256.
//
//   - SALT = nonceS ‖ nonceA (the two handshake nonces, RAW bytes, fixed 32 each)
//     binds the derived key to THIS exact handshake: a different nonce pair
//     yields an independent key, so no two handshakes share a session key even
//     in the impossible event of an ephemeral collision.
//   - INFO = hkdfInfo domain-separates this key from any other derivation.
//
// LOW-ORDER / ZERO REJECTION: a small-subgroup attack that forces a predictable
// (all-zero) shared secret is rejected — crypto/ecdh's X25519 ECDH returns an
// error for the all-zero output, and we propagate it (fail closed: no key, so
// the caller aborts the handshake and no channel ever lives).
func deriveSessionKey(ownEph *ecdh.PrivateKey, peerEph *ecdh.PublicKey, nonceSB64, nonceAB64 string) ([]byte, error) {
	shared, err := ownEph.ECDH(peerEph)
	if err != nil {
		return nil, fmt.Errorf("agentchan: X25519 ECDH failed (rejected zero/low-order point): %w", err)
	}
	saltS, err := base64.StdEncoding.DecodeString(nonceSB64)
	if err != nil {
		return nil, errors.New("agentchan: server nonce malformed for KDF salt")
	}
	saltA, err := base64.StdEncoding.DecodeString(nonceAB64)
	if err != nil {
		return nil, errors.New("agentchan: agent nonce malformed for KDF salt")
	}
	salt := make([]byte, 0, len(saltS)+len(saltA))
	salt = append(salt, saltS...)
	salt = append(salt, saltA...)

	r := hkdf.New(sha256.New, shared, salt, []byte(hkdfInfo))
	key := make([]byte, aeadKeyBytes)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("agentchan: HKDF expand session key: %w", err)
	}
	return key, nil
}

// makeNonce builds the 12-byte ChaCha20-Poly1305 nonce for one sealed frame:
//
//	byte 0      : direction tag (dirServerToAgent / dirAgentToServer)
//	bytes 1..3  : zero padding
//	bytes 4..11 : big-endian 64-bit frame counter
//
// The direction tag in the top byte makes the two directions' nonce spaces
// disjoint; the strictly-increasing counter makes every nonce within a direction
// unique. A 64-bit counter cannot realistically be exhausted, and the sealer
// aborts before it could wrap (see frameSealer.seal/open), so a nonce is never
// reused under the session key.
func makeNonce(dir byte, counter uint64) []byte {
	var nonce [aeadNonceBytes]byte
	nonce[0] = dir
	binary.BigEndian.PutUint64(nonce[aeadNonceBytes-8:], counter)
	return nonce[:]
}

// frameSealer AEAD-seals outbound frames and opens inbound frames for ONE
// channel under the shared session key. Each DIRECTION has an independent 64-bit
// counter starting at 0; the counter is the low 8 bytes of the nonce and the
// direction tag is the top byte (see makeNonce). Counters are IMPLICIT on the
// wire — only the ciphertext is transmitted. WebSocket runs over TCP, which
// delivers messages reliably and IN ORDER, so both peers keep their send/recv
// counters in lockstep with no counter bytes on the wire. Any tamper, injection,
// reorder, drop, or replay desynchronizes the stream: the AEAD tag is verified
// against the EXPECTED nonce, so a stale/forged/out-of-order ciphertext fails to
// open, and the channel is torn down before any such frame is processed.
type frameSealer struct {
	aead    cipher.AEAD
	sendDir byte
	recvDir byte

	sendMu  sync.Mutex
	sendCtr uint64

	recvMu  sync.Mutex
	recvCtr uint64
}

// newFrameSealer builds the per-channel sealer from the derived session key.
// side selects which direction this channel SENDS on ("s" = server/dialer sends
// server→agent, "a" = agent/listener sends agent→server); the opposite tag is
// used to open the peer's frames.
func newFrameSealer(sessionKey []byte, side string) (*frameSealer, error) {
	if len(sessionKey) != aeadKeyBytes {
		return nil, fmt.Errorf("agentchan: session key must be %d bytes, got %d", aeadKeyBytes, len(sessionKey))
	}
	aead, err := chacha20poly1305.New(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("agentchan: init ChaCha20-Poly1305: %w", err)
	}
	var sendDir, recvDir byte
	switch side {
	case "s":
		sendDir, recvDir = dirServerToAgent, dirAgentToServer
	case "a":
		sendDir, recvDir = dirAgentToServer, dirServerToAgent
	default:
		return nil, fmt.Errorf("agentchan: unknown channel side %q", side)
	}
	return &frameSealer{aead: aead, sendDir: sendDir, recvDir: recvDir}, nil
}

// seal encrypts one frame's plaintext under the next send nonce and advances the
// send counter. It MUST be called in the same order frames hit the wire (the
// channel serializes it under its write lock) so the receiver's implicit counter
// stays in step. OVERFLOW GUARD: it refuses to seal once the counter reaches its
// maximum, so the counter can never wrap and reuse a nonce (practically
// unreachable — 2^64 frames).
func (s *frameSealer) seal(plaintext []byte) ([]byte, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendCtr == math.MaxUint64 {
		return nil, errors.New("agentchan: send nonce counter exhausted — refusing to reuse a nonce")
	}
	nonce := makeNonce(s.sendDir, s.sendCtr)
	sealed := s.aead.Seal(nil, nonce, plaintext, nil)
	s.sendCtr++
	return sealed, nil
}

// open authenticates and decrypts one inbound sealed frame under the next
// expected recv nonce, advancing the recv counter ONLY on success. A failed tag
// (tamper / injection / reorder / replay) returns an error and leaves the
// counter untouched — the caller tears the channel down, so a desynchronized
// counter never matters. OVERFLOW GUARD mirrors seal.
func (s *frameSealer) open(ciphertext []byte) ([]byte, error) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	if s.recvCtr == math.MaxUint64 {
		return nil, errors.New("agentchan: recv nonce counter exhausted")
	}
	nonce := makeNonce(s.recvDir, s.recvCtr)
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("agentchan: AEAD open failed (tamper/inject/reorder/replay): %w", err)
	}
	s.recvCtr++
	return plaintext, nil
}
