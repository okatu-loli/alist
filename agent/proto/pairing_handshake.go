package proto

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// This file is the FIRST-CONTACT pairing handshake for TOKEN-AUTHENTICATED
// reverse mode (design subproject F, wave F1). It REPLACES the SP-E
// commit-before-reveal + human-compared SAS flow with a clean 3-message
// "reveal + tag" exchange whose ONLY anti-MITM anchor is a high-entropy
// one-time token T (see pairing_token.go) that both sides key an HMAC over the
// FULL handshake transcript with. It runs ONCE, when the server dials an
// as-yet-unknown agent's /agentpair endpoint, and its job is to let two parties
// that share T — but have NO pinned key yet — exchange their long-term ed25519
// public keys and each PROVE to the other, cryptographically, that it holds T
// over exactly the keys+nonces on the wire. It is transport-agnostic (plain
// io.ReadWriter, length-prefixed JSON) so it unit-tests over an in-memory pipe;
// F2/F3 wire it onto the real WS listener/dialer and the token lifecycle.
//
// WHY COMMIT-BEFORE-REVEAL IS GONE (design spec Critical #1): the SP-E commit
// existed to stop a two-session MITM from GRINDING nonces to force a matching
// SAS. Once authentication is a 256-bit HMAC tag over both pubkeys and both
// nonces, grinding buys nothing (the attacker still cannot forge a tag without
// T), so the commit contributes zero to authentication. Worse, keeping it would
// force tagS to be computed in M2 BEFORE the agent's nonce is known — i.e. over
// a PARTIAL transcript — which re-opens the asymmetric MITM the tag is meant to
// close. So the flow is the minimal reveal+tag one and every tag covers the
// COMPLETE transcript.
//
// THE 3-MESSAGE REVEAL+TAG FLOW:
//
//	M1 agent  → server : PairAgentHello   { AgentPub, NonceA }        (reveal; no tag — nonceS unknown yet)
//	M2 server → agent  : PairServerHello  { ServerPub, NonceS, TagS } (server now holds the FULL transcript)
//	M3 agent  → server : PairAgentConfirm { TagA }                    (agent computes the FULL transcript on M2)
//
// The agent speaks first even though the server dialed the socket: it reveals
// its pubkey + a fresh nonce. Only after M1 does the server hold both pubkeys
// and both nonces, so it is the first party able to compute the full transcript
// and therefore the first to emit a tag (tagS). The agent computes the same full
// transcript the instant it reads M2 and answers with tagA.
//
// WHY IT IS SECURE (design spec「为什么安全」, HOLDS on every axis):
//   - MUTUAL / anti-MITM: BOTH tags cover BOTH pubkeys AND BOTH nonces. A MITM
//     that swaps EITHER pubkey in flight makes the two sides' transcripts differ,
//     so the tag it must forge to keep the victim believing needs T — which it
//     does not have — and the victim's constant-time verify fails. Neither side
//     ever pins a key until it has verified the PEER's tag.
//   - OFFLINE: a tag is HMAC-SHA256(T, public values); with T ≥ 128 bits
//     (enforced at 256 in pairing_token.go) it cannot be brute-forced from a
//     captured transcript.
//   - RELAY / REPLAY: each tag binds THIS exchange's two fresh nonces. A tag
//     captured from another session is over a different transcript and will not
//     verify here — the verifier always contributes a FRESH crypto/rand nonce,
//     so a replayed peer message can never reproduce the expected tag. This is
//     why no separate seen-nonce cache is needed (unlike the signed SP-E flow):
//     freshness is intrinsic to the tag binding.
//
// WHY NO PER-MESSAGE SIGNATURE (dropped from SP-E): in the token model the tag
// IS the authentication. A party that cannot produce a valid tag has no T and is
// rejected; a party that CAN produce one already holds the shared secret and is
// authorized. An ed25519 key-possession signature would only prove "I hold the
// private half of the pubkey I just sent", which adds nothing to the anti-MITM
// property (that is the tag's job) — and if a token-holder presents a pubkey it
// does not control, the only consequence is that the later R2 pinned-key data
// channel cannot come up (a self-inflicted liveness failure, not a compromise).
// So the pubkeys travel unsigned; the token tag is the sole cryptographic gate.
//
// VERIFY-BEFORE-PIN is the load-bearing invariant and a TESTED one (design spec
// Critical #1 / Important #6): a side pins the peer's pubkey ONLY after a
// constant-time verify of the PEER's tag — never on the strength of "I sent my
// own tag". See RunAgentPairingHandshake / RunServerPairingHandshake.

const (
	// reversePairTranscriptDomain is the fixed domain label that PREFIXES every
	// transcript, separating a reverse-pairing tag preimage from any other hashed
	// or MAC'd value in this codebase. The "-v1" suffix reserves room to rotate
	// the construction. Byte layout of the full transcript (all fields fixed
	// length, order PINNED — see reversePairTranscript):
	//
	//	transcript = reversePairTranscriptDomain (ASCII, 21 bytes)
	//	           ‖ serverPub (32 bytes, ed25519)
	//	           ‖ agentPub  (32 bytes, ed25519)
	//	           ‖ nonceS    (32 bytes, crypto/rand)
	//	           ‖ nonceA    (32 bytes, crypto/rand)
	reversePairTranscriptDomain = "alist-rev-pair-tok-v1"

	// reversePairServerPrefix / reversePairAgentPrefix are the DISTINCT role
	// prefixes prepended (ahead of the transcript) inside each side's tag. Making
	// them different is what forces tagS != tagA even over an identical
	// transcript, so a tag from one role can never be reflected as the other's.
	reversePairServerPrefix = "alist-rev-pair-tok-server-v1"
	reversePairAgentPrefix  = "alist-rev-pair-tok-agent-v1"

	// roleServer / roleAgent select which role prefix reversePairTag uses.
	roleServer = "server"
	roleAgent  = "agent"

	// reversePairFieldBytes is the fixed length of EACH transcript field: an
	// ed25519 public key and a pairing nonce are both 32 bytes, so one constant
	// guards all four. A field of any other length is a hard error in
	// reversePairTranscript — a partial/short transcript must never be tag'd.
	reversePairFieldBytes = 32

	// Message framing tags. All authentication is in the token TAG over the
	// domain-separated transcript, never the Type — the Type only routes framing.
	pairAgentHelloType   = "pair_agent_hello"
	pairServerHelloType  = "pair_server_hello"
	pairAgentConfirmType = "pair_agent_confirm"

	// pairingNonceBytes is the length of each side's crypto/rand nonce (256
	// bits). Never a counter/time source: the nonces are what make each tag fresh
	// to THIS exchange (anti-replay/anti-precompute).
	pairingNonceBytes = 32

	// maxPairingHandshakeBytes bounds a single pairing message on this
	// anonymous-until-verified inbound surface. A hello/confirm is a few hundred
	// bytes; a small cap keeps an unauthenticated peer from streaming a huge frame
	// into memory before it has proven anything.
	maxPairingHandshakeBytes = 8 << 10

	// PairingStepTimeout bounds each blocking read/write so a peer that connects
	// and then stalls cannot hold the exchange (and a goroutine) open
	// indefinitely. Applied only when the underlying conn supports deadlines (see
	// pairingDeadliner).
	PairingStepTimeout = 15 * time.Second
)

// PairAgentHello is M1 (agent → server): the agent REVEALS its long-term public
// key and a fresh nonce. It carries NO tag — at this point the agent has not yet
// seen the server's nonce, so it cannot compute the full transcript. Sending a
// tag here would force it over a partial transcript, which is exactly the mistake
// the reveal+tag flow avoids.
type PairAgentHello struct {
	Type     string `json:"type"`      // always pairAgentHelloType
	AgentPub string `json:"agent_pub"` // base64(std) ed25519 public key (32B)
	NonceA   string `json:"nonce_a"`   // base64(std) of pairingNonceBytes
}

// PairServerHello is M2 (server → agent): after M1 the server holds BOTH
// pubkeys and BOTH nonces, so it computes the full transcript and proves it
// holds T with TagS = reversePairTag(T, roleServer, transcript).
type PairServerHello struct {
	Type      string `json:"type"`       // always pairServerHelloType
	ServerPub string `json:"server_pub"` // base64(std) ed25519 public key (32B)
	NonceS    string `json:"nonce_s"`    // base64(std) of pairingNonceBytes
	TagS      string `json:"tag_s"`      // base64(std) HMAC-SHA256 tag (32B)
}

// PairAgentConfirm is M3 (agent → server): on receiving M2 the agent computes
// the SAME full transcript and answers with TagA = reversePairTag(T, roleAgent,
// transcript). The agent sends this ONLY after its own constant-time verify of
// TagS has passed — a wrong-token peer never even sees a TagA.
type PairAgentConfirm struct {
	Type string `json:"type"`  // always pairAgentConfirmType
	TagA string `json:"tag_a"` // base64(std) HMAC-SHA256 tag (32B)
}

// reversePairTranscript assembles the domain-separated transcript both sides
// tag over:
//
//	reversePairTranscriptDomain ‖ serverPub ‖ agentPub ‖ nonceS ‖ nonceA
//
// EVERY field must be exactly reversePairFieldBytes (32); a field of any other
// length is a HARD ERROR. This is the code-level guard behind the spec rule that
// a tag must never be computed over a partial/short transcript (design spec
// Critical #1): the honest paths always pass four full 32-byte fields, and a
// misuse that tried to tag a truncated transcript is refused here rather than
// silently weakening the binding.
func reversePairTranscript(serverPub, agentPub, nonceS, nonceA []byte) ([]byte, error) {
	for _, f := range [][]byte{serverPub, agentPub, nonceS, nonceA} {
		if len(f) != reversePairFieldBytes {
			return nil, fmt.Errorf("agentpair: transcript field must be %d bytes (got %d) — refusing to tag a partial transcript", reversePairFieldBytes, len(f))
		}
	}
	buf := make([]byte, 0, len(reversePairTranscriptDomain)+4*reversePairFieldBytes)
	buf = append(buf, reversePairTranscriptDomain...)
	buf = append(buf, serverPub...)
	buf = append(buf, agentPub...)
	buf = append(buf, nonceS...)
	buf = append(buf, nonceA...)
	return buf, nil
}

// reversePairRolePrefix maps a role selector to its distinct tag prefix. An
// unknown role is a programmer error on an internal call, so it panics rather
// than silently producing a tag under the wrong (or empty) domain — a tag under
// a mistaken prefix would be a silent authentication break.
func reversePairRolePrefix(role string) string {
	switch role {
	case roleServer:
		return reversePairServerPrefix
	case roleAgent:
		return reversePairAgentPrefix
	default:
		panic("agentpair: reversePairTag called with unknown role " + role)
	}
}

// reversePairTag computes a side's authentication tag:
//
//	HMAC-SHA256( tokenRaw, rolePrefix(role) ‖ transcript )
//
// The HMAC KEY is the 32 raw token bytes; the message is the role prefix
// followed by the COMPLETE transcript. Because the two roles use distinct
// prefixes, tagS and tagA differ even over an identical transcript, blocking
// reflection. transcript MUST be the full transcript from reversePairTranscript
// (both pubkeys + both nonces); passing a partial one is the exact MITM-reopening
// mistake the design forbids.
func reversePairTag(tokenRaw []byte, role string, transcript []byte) []byte {
	mac := hmac.New(sha256.New, tokenRaw)
	mac.Write([]byte(reversePairRolePrefix(role)))
	mac.Write(transcript)
	return mac.Sum(nil)
}

// pairingDeadliner (wsconn.go) is the optional deadline slice of a conn
// (net.Conn satisfies it). The handshake applies PairingStepTimeout when the
// conn supports it and silently skips deadlines otherwise (e.g. a bare pipe
// in a unit test) — the frame-size cap still bounds memory regardless.

func randPairingNonce() ([]byte, error) {
	b := make([]byte, pairingNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("agentpair: read random nonce: %w", err)
	}
	return b, nil
}

// writePairingJSON writes v as one length-prefixed (4-byte big-endian) JSON
// frame, capped at maxPairingHandshakeBytes.
func writePairingJSON(conn io.ReadWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentpair: encode handshake message: %w", err)
	}
	if len(b) > maxPairingHandshakeBytes {
		return fmt.Errorf("agentpair: handshake message too large (%d > %d)", len(b), maxPairingHandshakeBytes)
	}
	if d, ok := conn.(pairingDeadliner); ok {
		if err := d.SetWriteDeadline(time.Now().Add(PairingStepTimeout)); err != nil {
			return err
		}
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := conn.Write(hdr[:]); err != nil {
		return fmt.Errorf("agentpair: write handshake length: %w", err)
	}
	if _, err := conn.Write(b); err != nil {
		return fmt.Errorf("agentpair: write handshake message: %w", err)
	}
	return nil
}

// readPairingJSON reads one length-prefixed JSON frame and unmarshals it. The
// declared length is checked against maxPairingHandshakeBytes BEFORE any payload
// buffer is allocated, so an oversized frame is rejected on the header alone.
func readPairingJSON(conn io.ReadWriter, v any) error {
	if d, ok := conn.(pairingDeadliner); ok {
		if err := d.SetReadDeadline(time.Now().Add(PairingStepTimeout)); err != nil {
			return err
		}
	}
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return fmt.Errorf("agentpair: read handshake length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("agentpair: handshake message is empty")
	}
	if n > maxPairingHandshakeBytes {
		return fmt.Errorf("agentpair: handshake message too large (%d > %d)", n, maxPairingHandshakeBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("agentpair: read handshake message: %w", err)
	}
	if err := json.Unmarshal(buf, v); err != nil {
		return fmt.Errorf("agentpair: decode handshake message: %w", err)
	}
	return nil
}

// decodePairingPubKey base64-decodes a presented public key and enforces the
// ed25519 public-key size, so every downstream transcript/tag runs on a
// well-formed 32-byte key rather than an attacker-chosen length.
func decodePairingPubKey(b64 string) ([]byte, error) {
	pub, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("agentpair: public key is malformed base64")
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("agentpair: public key has wrong length (%d, want %d)", len(pub), ed25519.PublicKeySize)
	}
	return pub, nil
}

// decodePairingNonce base64-decodes a nonce and enforces the fixed nonce size.
func decodePairingNonce(b64 string) ([]byte, error) {
	n, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("agentpair: nonce is malformed base64")
	}
	if len(n) != pairingNonceBytes {
		return nil, fmt.Errorf("agentpair: nonce has wrong length (%d, want %d)", len(n), pairingNonceBytes)
	}
	return n, nil
}

// decodePairingTag base64-decodes a peer's tag and enforces the HMAC-SHA256
// digest size, so a malformed/short tag is rejected before any comparison.
func decodePairingTag(b64 string) ([]byte, error) {
	tag, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("agentpair: tag is malformed base64")
	}
	if len(tag) != sha256.Size {
		return nil, fmt.Errorf("agentpair: tag has wrong length (%d, want %d)", len(tag), sha256.Size)
	}
	return tag, nil
}

// RunServerPairingHandshake performs the SERVER/dialer leg over conn. Even
// though the server dialed the socket, the AGENT speaks the first PROTOCOL
// frame: the server READS M1 (PairAgentHello: agentPub + nonceA), picks a fresh
// nonceS, computes the FULL transcript, and SENDS M2 (PairServerHello) carrying
// its own tagS. It then READS M3 (PairAgentConfirm) and CONSTANT-TIME-VERIFIES
// the agent's tagA over that same full transcript. It returns the agent's
// presented public key + derived agentID ONLY after tagA verifies — never on the
// strength of having sent its own tagS (verify-before-pin). tokenRaw is the 32
// raw token bytes both sides share. There is NO pinned-key check — this is TOFU;
// the token tag is the authentication.
func RunServerPairingHandshake(conn io.ReadWriter, serverPub, tokenRaw []byte) (agentPub []byte, agentID string, err error) {
	if len(serverPub) != ed25519.PublicKeySize {
		return nil, "", errors.New("agentpair: server public key is missing/malformed")
	}
	if err := ValidatePairingToken(tokenRaw); err != nil {
		return nil, "", err
	}

	// 1. READ M1 — the agent reveals its pubkey + nonceA (no tag yet).
	var ah PairAgentHello
	if err := readPairingJSON(conn, &ah); err != nil {
		return nil, "", err
	}
	if ah.Type != pairAgentHelloType {
		return nil, "", fmt.Errorf("agentpair: unexpected message type (expected %q)", pairAgentHelloType)
	}
	agentPub, err = decodePairingPubKey(ah.AgentPub)
	if err != nil {
		return nil, "", err
	}
	nonceA, err := decodePairingNonce(ah.NonceA)
	if err != nil {
		return nil, "", err
	}

	// 2. Now the server holds BOTH pubkeys and BOTH nonces → compute the FULL
	//    transcript and SEND M2 with our tagS over it.
	nonceS, err := randPairingNonce()
	if err != nil {
		return nil, "", err
	}
	transcript, err := reversePairTranscript(serverPub, agentPub, nonceS, nonceA)
	if err != nil {
		return nil, "", err
	}
	tagS := reversePairTag(tokenRaw, roleServer, transcript)
	hello := PairServerHello{
		Type:      pairServerHelloType,
		ServerPub: base64.StdEncoding.EncodeToString(serverPub),
		NonceS:    base64.StdEncoding.EncodeToString(nonceS),
		TagS:      base64.StdEncoding.EncodeToString(tagS),
	}
	if err := writePairingJSON(conn, hello); err != nil {
		return nil, "", err
	}

	// 3. READ M3 and VERIFY the agent's tagA BEFORE returning the key to pin.
	var ac PairAgentConfirm
	if err := readPairingJSON(conn, &ac); err != nil {
		return nil, "", err
	}
	if ac.Type != pairAgentConfirmType {
		return nil, "", fmt.Errorf("agentpair: unexpected message type (expected %q)", pairAgentConfirmType)
	}
	gotTagA, err := decodePairingTag(ac.TagA)
	if err != nil {
		return nil, "", err
	}
	wantTagA := reversePairTag(tokenRaw, roleAgent, transcript)
	// VERIFY-BEFORE-PIN: constant-time compare the PEER's tag. Only a peer that
	// holds the same T over this exact transcript can produce it; a wrong token,
	// a swapped pubkey, or a relayed tag from another session all fail here and
	// NOTHING is pinned.
	if subtle.ConstantTimeCompare(gotTagA, wantTagA) != 1 {
		return nil, "", errors.New("agentpair: agent tag failed verification (wrong token or tampered/relayed transcript)")
	}

	return agentPub, FingerprintPubKey(agentPub), nil
}

// RunAgentPairingHandshake performs the AGENT/listener leg over conn. The agent
// speaks first: it picks a fresh nonceA and SENDS M1 (PairAgentHello) revealing
// its pubkey + nonceA. It then READS M2 (PairServerHello), computes the FULL
// transcript, and CONSTANT-TIME-VERIFIES the server's tagS over it BEFORE it
// pins serverPub or sends anything further. Only on a passing verify does it send
// M3 (PairAgentConfirm) with its own tagA and RETURN the server's public key to
// pin. A failing verify returns an error and pins NOTHING — no M3 is sent, so a
// wrong-token peer never obtains a TagA (verify-before-pin). tokenRaw is the 32
// raw token bytes both sides share. There is NO pinned-key check — this is TOFU.
func RunAgentPairingHandshake(conn io.ReadWriter, agentPub, tokenRaw []byte) (serverPub []byte, err error) {
	if len(agentPub) != ed25519.PublicKeySize {
		return nil, errors.New("agentpair: agent public key is missing/malformed")
	}
	if err := ValidatePairingToken(tokenRaw); err != nil {
		return nil, err
	}

	// 1. Reveal: pick nonceA and SEND M1.
	nonceA, err := randPairingNonce()
	if err != nil {
		return nil, err
	}
	hello := PairAgentHello{
		Type:     pairAgentHelloType,
		AgentPub: base64.StdEncoding.EncodeToString(agentPub),
		NonceA:   base64.StdEncoding.EncodeToString(nonceA),
	}
	if err := writePairingJSON(conn, hello); err != nil {
		return nil, err
	}

	// 2. READ M2 (serverPub + nonceS + tagS).
	var sh PairServerHello
	if err := readPairingJSON(conn, &sh); err != nil {
		return nil, err
	}
	if sh.Type != pairServerHelloType {
		return nil, fmt.Errorf("agentpair: unexpected message type (expected %q)", pairServerHelloType)
	}
	serverPub, err = decodePairingPubKey(sh.ServerPub)
	if err != nil {
		return nil, err
	}
	nonceS, err := decodePairingNonce(sh.NonceS)
	if err != nil {
		return nil, err
	}
	gotTagS, err := decodePairingTag(sh.TagS)
	if err != nil {
		return nil, err
	}

	// VERIFY-BEFORE-PIN: compute the FULL transcript and constant-time-verify the
	// server's tagS BEFORE pinning serverPub or sending M3. A wrong token or a
	// MITM-swapped serverPub makes this fail; we return an error, send NO M3, and
	// pin NOTHING (the caller keeps the acceptor armed).
	transcript, err := reversePairTranscript(serverPub, agentPub, nonceS, nonceA)
	if err != nil {
		return nil, err
	}
	wantTagS := reversePairTag(tokenRaw, roleServer, transcript)
	if subtle.ConstantTimeCompare(gotTagS, wantTagS) != 1 {
		return nil, errors.New("agentpair: server tag failed verification (wrong token or tampered serverPub/nonce)")
	}

	// 3. tagS verified → the server holds T. Answer with our tagA, THEN return
	//    serverPub for the caller to pin.
	tagA := reversePairTag(tokenRaw, roleAgent, transcript)
	confirm := PairAgentConfirm{
		Type: pairAgentConfirmType,
		TagA: base64.StdEncoding.EncodeToString(tagA),
	}
	if err := writePairingJSON(conn, confirm); err != nil {
		return nil, err
	}
	return serverPub, nil
}
