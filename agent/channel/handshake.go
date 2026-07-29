package channel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

// This file is the mutual-auth handshake for the reverse-connection channel
// (design R2). It runs ONCE, right after the WebSocket upgrade, BEFORE any
// Frame is processed. Only if BOTH legs verify does the caller wrap the conn
// in a live Channel; any failure returns an error and the caller closes the
// conn without ever dispatching a frame.
//
// Two ed25519 signatures, one per direction, each domain-separated by a
// distinct constant prefix so a signature minted for one leg can never be
// replayed as the other, and both prefixes differ from the heartbeat's
// (prefix-less) signing message (agent.HeartbeatMessage) so a captured
// heartbeat signature can't be reused here and vice-versa. As of R2.5 each
// signature ALSO covers that side's fresh X25519 ephemeral public key:
//
//	sig_s = Sign(serverPriv, "agentchan-server|"+agentID+"|"+nonceS+"|"+eph_s+"|"+ts_s)
//	sig_a = Sign(agentPriv,  "agentchan-agent|"+agentID+"|"+nonceS+"|"+nonceA+"|"+eph_a+"|"+ts_a)
//
// What each binds / the replay defense:
//   - The SERVER (dialer) speaks first with ServerHello: a fresh crypto/rand
//     nonceS, a fresh X25519 ephemeral public key eph_s, a timestamp ts_s, and
//     sig_s over them. The agent verifies sig_s against the server public key it
//     PINNED at pairing time (fail closed if it has none) and checks ts_s
//     freshness (±handshakeFreshness) so a captured ServerHello replayed later
//     is rejected as stale.
//   - The AGENT (listener) replies AgentHello: its own fresh nonceA, its fresh
//     ephemeral eph_a, ts_a, and sig_a that BINDS nonceS. Because nonceS is
//     generated fresh by the server for THIS handshake, the agent's signature
//     cannot be pre-computed or lifted from an earlier handshake — it is fresh
//     to this exact exchange (channel binding). The server verifies sig_a
//     against the agent's stored public key and checks ts_a freshness.
//   - The two nonces + the two timestamps are the replay defense; the agent
//     also keeps an optional seen-nonceS cache (defense in depth, mirroring
//     the heartbeat's seen-nonce cache) so even a within-window ServerHello
//     replay is refused.
//
// R2.5 — AUTHENTICATED EPHEMERAL ECDH: because each ephemeral public key is
// covered by that side's long-term ed25519 signature, a network MITM on a plain
// ws:// link cannot substitute an ephemeral without breaking a signature. After
// BOTH signatures verify AND both ephemerals parse as valid X25519 points, each
// side runs X25519(own_eph_priv, peer_eph_pub) to the same shared secret and
// HKDF-SHA256(secret, salt=nonceS‖nonceA, info="agentchan-aead-v1") derives the
// 32-byte session key the Channel uses to AEAD-seal every frame (see crypto.go).
// The ephemerals are fresh per handshake and never persisted → forward secrecy.
// Both handshake functions RETURN that session key; the hellos themselves stay
// plaintext JSON (they carry only PUBLIC ephemerals + authenticated signatures,
// no secret).
//
// The server's "is this agent known and Enabled?" check is NOT here — the
// server is the dialer and resolves the agent row (pubkey + Enabled) BEFORE
// dialing (see Manager); this file only performs the cryptographic legs.

const (
	// serverHelloPrefix / agentHelloPrefix domain-separate the two handshake
	// signatures from each other AND from the heartbeat's prefix-less message
	// (agent.HeartbeatMessage builds "agentID|ts|nonce"). A signature over one
	// of these can never verify against another message shape, so no signature
	// is reusable across legs or across the heartbeat/handshake boundary.
	serverHelloPrefix = "agentchan-server"
	agentHelloPrefix  = "agentchan-agent"

	// handshakeFreshness is how far a hello's unix timestamp may drift from the
	// verifier's clock (either direction) before it is rejected as stale —
	// mirrors internal/agent's heartbeatFreshness. Wide enough for real clock
	// skew, narrow enough that a captured hello is only replayable briefly even
	// before the nonce cache is consulted.
	handshakeFreshness = 3 * time.Minute

	// handshakeNonceBytes is the length of each side's crypto/rand nonce. 32
	// bytes (256 bits) makes an accidental nonce collision impossible and is
	// generated from crypto/rand, never a counter/time source.
	handshakeNonceBytes = 32

	// maxHandshakeBytes bounds a single handshake message on this NEW,
	// anonymous-until-verified inbound surface: a hello is a few hundred bytes,
	// so a small cap keeps an unauthenticated peer from streaming a huge frame
	// into memory before it has proven anything.
	maxHandshakeBytes = 8 << 10

	// handshakeStepTimeout bounds each blocking read/write of the handshake so
	// a peer that connects and then stalls cannot hold the exchange (and, on
	// the listener, a goroutine) open indefinitely.
	handshakeStepTimeout = 15 * time.Second
)

// ServerHello is the dialer→listener opening message. Type is a plain framing
// tag; the security is entirely in Sig over the domain-separated message.
type ServerHello struct {
	Type    string `json:"type"` // always "server_hello"
	AgentID string `json:"agent_id"`
	NonceS  string `json:"nonce_s"` // base64(std) of handshakeNonceBytes
	EphS    string `json:"eph_s"`   // base64(std) X25519 ephemeral PUBLIC key (fresh per dial); authenticated by sig_s
	Ts      int64  `json:"ts"`
	Sig     string `json:"sig_s"` // base64(std) of sig_s (covers eph_s)
}

// AgentHello is the listener→dialer reply. Sig binds nonceS (from ServerHello)
// so it is fresh to this handshake, and covers eph_a.
type AgentHello struct {
	Type   string `json:"type"` // always "agent_hello"
	NonceA string `json:"nonce_a"`
	EphA   string `json:"eph_a"` // base64(std) X25519 ephemeral PUBLIC key (fresh per handshake); authenticated by sig_a
	Ts     int64  `json:"ts"`
	Sig    string `json:"sig_a"` // base64(std) of sig_a (covers eph_a)
}

const (
	serverHelloType = "server_hello"
	agentHelloType  = "agent_hello"
)

// serverHelloSigningMessage is the exact byte sequence the server signs and the
// agent verifies for the server leg. Domain-separated by serverHelloPrefix and
// (R2.5) binding the server's ephemeral public key ephS so a MITM cannot
// substitute an ephemeral without invalidating sig_s.
func serverHelloSigningMessage(agentID, nonceS, ephS string, ts int64) []byte {
	return []byte(serverHelloPrefix + "|" + agentID + "|" + nonceS + "|" + ephS + "|" + strconv.FormatInt(ts, 10))
}

// agentHelloSigningMessage is the exact byte sequence the agent signs and the
// server verifies for the agent leg. It BINDS nonceS (the server's fresh nonce)
// in addition to the agent's own nonceA — channel binding / anti-replay — and
// (R2.5) binds the agent's ephemeral public key ephA.
func agentHelloSigningMessage(agentID, nonceS, nonceA, ephA string, ts int64) []byte {
	return []byte(agentHelloPrefix + "|" + agentID + "|" + nonceS + "|" + nonceA + "|" + ephA + "|" + strconv.FormatInt(ts, 10))
}

// SeenNonceFunc reports whether a nonce has already been observed (and records
// it). It is the agent listener's optional replay cache. Returning true means
// "already seen — reject". A nil SeenNonceFunc disables the extra check
// (freshness + the fresh server nonce still bound the exchange).
type SeenNonceFunc func(nonce string) (seen bool)

func randNonce() (string, error) {
	b := make([]byte, handshakeNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("agentchan: read random nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func fresh(ts int64, now time.Time) bool {
	delta := now.Unix() - ts
	if delta < 0 {
		delta = -delta
	}
	return time.Duration(delta)*time.Second <= handshakeFreshness
}

// writeHandshakeJSON writes v as a single bounded, deadlined TEXT message.
func writeHandshakeJSON(conn wsConn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentchan: encode handshake message: %w", err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(handshakeStepTimeout)); err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		return fmt.Errorf("agentchan: write handshake message: %w", err)
	}
	return nil
}

// readHandshakeJSON reads one bounded, deadlined message and unmarshals it.
func readHandshakeJSON(conn wsConn, v any) error {
	conn.SetReadLimit(maxHandshakeBytes)
	if err := conn.SetReadDeadline(time.Now().Add(handshakeStepTimeout)); err != nil {
		return err
	}
	mt, data, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("agentchan: read handshake message: %w", err)
	}
	if mt != websocket.TextMessage {
		return errors.New("agentchan: handshake message was not a text frame")
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("agentchan: decode handshake message: %w", err)
	}
	return nil
}

// RunServerHandshake performs the SERVER/dialer leg over conn: it generates a
// fresh X25519 ephemeral, sends a signed ServerHello carrying its public half,
// verifies the agent's reply against agentPub, and derives the AEAD session key
// from the authenticated ephemeral ECDH. On success it returns the 32-byte
// session key and the caller wraps conn in a Channel (side "s"); on any error
// the caller MUST close conn without processing frames. now is injectable for
// tests.
func RunServerHandshake(conn wsConn, serverPriv ed25519.PrivateKey, agentPub ed25519.PublicKey, agentID string, now time.Time) (sessionKey []byte, err error) {
	if len(serverPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("agentchan: server private key is missing/malformed")
	}
	if len(agentPub) != ed25519.PublicKeySize {
		return nil, errors.New("agentchan: agent public key is missing/malformed")
	}
	if agentID == "" {
		return nil, errors.New("agentchan: agentID is required")
	}

	// Fresh ephemeral per dial → forward secrecy. Its public half is signed
	// (below) so a MITM cannot swap it.
	ownEph, err := generateEphemeral()
	if err != nil {
		return nil, err
	}
	ephS := ephemeralPublicB64(ownEph)

	nonceS, err := randNonce()
	if err != nil {
		return nil, err
	}
	ts := now.Unix()
	sigS := ed25519.Sign(serverPriv, serverHelloSigningMessage(agentID, nonceS, ephS, ts))
	hello := ServerHello{
		Type:    serverHelloType,
		AgentID: agentID,
		NonceS:  nonceS,
		EphS:    ephS,
		Ts:      ts,
		Sig:     base64.StdEncoding.EncodeToString(sigS),
	}
	if err := writeHandshakeJSON(conn, hello); err != nil {
		return nil, err
	}

	var ah AgentHello
	if err := readHandshakeJSON(conn, &ah); err != nil {
		return nil, err
	}
	if ah.Type != agentHelloType {
		return nil, fmt.Errorf("agentchan: expected %q, got %q", agentHelloType, ah.Type)
	}
	if !fresh(ah.Ts, now) {
		return nil, errors.New("agentchan: agent hello timestamp is stale (possible replay)")
	}
	if ah.NonceA == "" {
		return nil, errors.New("agentchan: agent hello is missing its nonce")
	}
	if ah.EphA == "" {
		return nil, errors.New("agentchan: agent hello is missing its ephemeral key")
	}
	sigA, err := base64.StdEncoding.DecodeString(ah.Sig)
	if err != nil {
		return nil, errors.New("agentchan: agent hello signature is malformed")
	}
	// The agent's signature binds OUR fresh nonceS AND its ephemeral eph_a — a
	// signature lifted from a different handshake, or over a substituted
	// ephemeral, cannot verify here. Signature verification therefore
	// AUTHENTICATES ah.EphA before we ever use it in ECDH.
	if !ed25519.Verify(agentPub, agentHelloSigningMessage(agentID, nonceS, ah.NonceA, ah.EphA, ah.Ts), sigA) {
		return nil, errors.New("agentchan: agent hello signature failed verification")
	}
	// eph_a is now authenticated; parse it as a valid X25519 point and derive
	// the session key. deriveSessionKey fails closed on a zero/low-order result.
	peerEph, err := parseEphemeral(ah.EphA)
	if err != nil {
		return nil, err
	}
	sessionKey, err = deriveSessionKey(ownEph, peerEph, nonceS, ah.NonceA)
	if err != nil {
		return nil, err
	}
	return sessionKey, nil
}

// RunAgentHandshake performs the AGENT/listener leg over conn: it verifies the
// server's ServerHello against the PINNED server public key (failing closed if
// none is pinned), replies with a signed AgentHello binding the server's nonce
// and carrying its own fresh ephemeral, and derives the AEAD session key from
// the authenticated ephemeral ECDH. On success it returns the 32-byte session
// key and the caller wraps conn in a Channel (side "a"); on any error the caller
// MUST close conn. seen may be nil (disables the extra replay cache). now is
// injectable for tests.
func RunAgentHandshake(conn wsConn, agentPriv ed25519.PrivateKey, agentID string, pinnedServerPub ed25519.PublicKey, now time.Time, seen SeenNonceFunc) (sessionKey []byte, err error) {
	// Fail closed: an agent with no pinned server key refuses reverse
	// connections outright — there is no key to verify the server against, so
	// no inbound connection can ever be trusted.
	if len(pinnedServerPub) != ed25519.PublicKeySize {
		return nil, errors.New("agentchan: no pinned server public key — refusing reverse connection (fail closed)")
	}
	if len(agentPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("agentchan: agent private key is missing/malformed")
	}
	if agentID == "" {
		return nil, errors.New("agentchan: agentID is required")
	}

	var sh ServerHello
	if err := readHandshakeJSON(conn, &sh); err != nil {
		return nil, err
	}
	if sh.Type != serverHelloType {
		return nil, fmt.Errorf("agentchan: expected %q, got %q", serverHelloType, sh.Type)
	}
	// The hello must be addressed to THIS agent: agent_id is bound into sig_s,
	// so a hello minted for a different agent cannot be verified here anyway,
	// but checking explicitly gives a clear rejection.
	if sh.AgentID != agentID {
		return nil, errors.New("agentchan: server hello is addressed to a different agent")
	}
	if !fresh(sh.Ts, now) {
		return nil, errors.New("agentchan: server hello timestamp is stale (possible replay)")
	}
	if sh.NonceS == "" {
		return nil, errors.New("agentchan: server hello is missing its nonce")
	}
	if sh.EphS == "" {
		return nil, errors.New("agentchan: server hello is missing its ephemeral key")
	}
	sigS, err := base64.StdEncoding.DecodeString(sh.Sig)
	if err != nil {
		return nil, errors.New("agentchan: server hello signature is malformed")
	}
	// Pinning enforced: verify against the key handed over at pairing time. A
	// hello signed by any other key (including a valid-but-different server
	// identity) fails here and the channel never lives. The signature covers
	// eph_s, so verification AUTHENTICATES the server's ephemeral before ECDH.
	if !ed25519.Verify(pinnedServerPub, serverHelloSigningMessage(agentID, sh.NonceS, sh.EphS, sh.Ts), sigS) {
		return nil, errors.New("agentchan: server hello signature failed verification against the pinned server key")
	}
	// eph_s is now authenticated; parse it as a valid X25519 point.
	peerEph, err := parseEphemeral(sh.EphS)
	if err != nil {
		return nil, err
	}
	// Defense in depth: refuse a within-window replay of a ServerHello whose
	// nonce we have already accepted.
	if seen != nil && seen(sh.NonceS) {
		return nil, errors.New("agentchan: server hello nonce already seen (replay)")
	}

	// Fresh ephemeral per handshake → forward secrecy; signed below so it cannot
	// be substituted in flight.
	ownEph, err := generateEphemeral()
	if err != nil {
		return nil, err
	}
	ephA := ephemeralPublicB64(ownEph)

	nonceA, err := randNonce()
	if err != nil {
		return nil, err
	}
	ts := now.Unix()
	sigA := ed25519.Sign(agentPriv, agentHelloSigningMessage(agentID, sh.NonceS, nonceA, ephA, ts))
	reply := AgentHello{
		Type:   agentHelloType,
		NonceA: nonceA,
		EphA:   ephA,
		Ts:     ts,
		Sig:    base64.StdEncoding.EncodeToString(sigA),
	}
	if err := writeHandshakeJSON(conn, reply); err != nil {
		return nil, err
	}
	sessionKey, err = deriveSessionKey(ownEph, peerEph, sh.NonceS, nonceA)
	if err != nil {
		return nil, err
	}
	return sessionKey, nil
}
