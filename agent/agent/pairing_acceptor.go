package agent

import (
	"encoding/base64"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Xhofe/go-cache"
	"github.com/gorilla/websocket"

	proto "github.com/AlistGo/alist-agent/proto"
)

// This file is the AGENT (listener) side of TOKEN-AUTHENTICATED FIRST-CONTACT
// reverse pairing (design subproject F). It is the opt-in, bounded,
// single-success acceptor that runs the pairing handshake
// (proto.RunAgentPairingHandshake, in pairing_handshake.go) on the agent's own serve
// listener, so a server with no public IP can DIAL the agent to pair. The
// anti-MITM anchor is the one-time token T the operator pastes into the pairing
// web (pairing_web.go): a dial-in that cannot produce a valid tag over the same
// T fails the handshake, pins NOTHING, and leaves the acceptor armed. On a
// PASSING token verify the acceptor PINS the server's public key into
// agent_config.json (0600) and lets the data channel go live — the crypto verify
// IS the consent, so there is no separate human compare step. The Confirm gate
// is retained only as an injectable seam (wired to return true in the token
// flow); the string it carries is the NON-LOAD-BEARING server-key fingerprint,
// shown in the web status purely for optional eyeballing.
//
// WHY IT LIVES IN PACKAGE agent (import-direction decision): the handshake it
// must run (proto.RunAgentPairingHandshake) is in package agent, and package agent
// already imports internal/agent/channel (see reverse.go). Putting the acceptor
// in package channel would make channel import agent → an import CYCLE. So the
// acceptor lives here, next to the handshake, and imports gorilla/websocket +
// go-cache DIRECTLY (mirroring channel's listener) rather than the other way
// around. The cmd layer (cmd/agent.go) wires it onto the serve mux.
//
// 🔒 SECURITY — this is a NEW anonymous-until-token-verified inbound surface, so
// it is bounded on every axis, mirroring channel's pinned-key data listener:
//
//   - OPT-IN + ARMED ONLY WITH A PASTED TOKEN: it is mounted ONLY under `alist
//     agent serve --pair` and is armed ONLY once the operator pastes a token T
//     into the pairing web (--pair-web). Until then it is disarmed and refuses
//     every dial. A long-running acceptor that never pairs must not be a standing
//     anonymous surface — that is why it (a) is opt-in, (b) arms only on a pasted
//     T, (c) AUTO-DISARMS after the first successful pairing, and (d) AUTO-DISARMS
//     when the token TTL expires (Disarm clears T). It must not stay open
//     indefinitely.
//   - SINGLE SUCCESS / AUTO-DISARM: after ONE verified pairing pins a server key
//     the acceptor stops accepting further pairing (armed=false, T cleared). A
//     failed handshake pins nothing and leaves it ARMED so a legitimate peer can
//     still pair within the TTL. A first-contact surface has no business accepting
//     a second unknown peer once one is pinned.
//   - SINGLE IN-FLIGHT: at most one pairing attempt runs at a time (inFlight
//     flag under mu). Concurrent connections cannot race to double-pin; the
//     losers are rejected before the WS upgrade.
//   - WS-ONLY: anything that is not a WebSocket upgrade is rejected.
//   - THROTTLED: a per-IP throttle (mirrors channel.handshakeThrottle: 20/min)
//     blunts a handshake flood; a self-contained go-cache so the agent needs no
//     server config.
//   - SIZE / STEP / FRESHNESS BOUNDS: inherited from the handshake
//     (maxPairingHandshakeBytes, pairingStepTimeout, the intrinsic nonce
//     freshness) plus a WS read limit on the adapter.
//
// The agent stays INERT until a dial-in produces a valid token tag: a wrong-token
// or MITM handshake fails the verify, pins NOTHING, and brings NO data channel
// live — "pin ≠ live without a valid T" is enforced HERE, agent-side, not only
// server-side. A hostile first-pairer that reaches /agentpair without T cannot
// complete the handshake at all. Only on a passing verify does the R2 pinned-key
// data listener come up; it then still refuses any server it cannot verify
// against the pinned key, and the server-side supervisor still refuses an
// unconfirmed/disabled agent (fail closed, defense in depth).

// pairThrottle rate-limits pairing attempts per client IP on this
// anonymous-until-confirmed inbound surface. It is an exact mirror of
// channel.handshakeThrottle (20 attempts / minute, self-contained go-cache);
// duplicated here rather than shared because channel's copy is unexported and
// importing channel's internals the other way would recreate the cycle this
// file's placement avoids.
type pairThrottle struct {
	cache  cache.ICache[int]
	max    int
	window time.Duration
}

func newPairThrottle() *pairThrottle {
	return &pairThrottle{cache: cache.NewMemCache[int](), max: 20, window: time.Minute}
}

func (t *pairThrottle) blocked(ip string) bool {
	n, ok := t.cache.Get(ip)
	return ok && n >= t.max
}

func (t *pairThrottle) record(ip string) {
	n, _ := t.cache.Get(ip)
	t.cache.Set(ip, n+1, cache.WithEx[int](t.window))
}

// PairingAcceptor is the agent's first-contact pairing HTTP handler (design E2).
// Build it with NewPairingAcceptor and mount its ServeHTTP on a single route
// (e.g. /agentpair) ONLY under `serve --pair`. It is armed on construction and
// disarms itself after the first successful pairing.
type PairingAcceptor struct {
	identity *Identity
	dataDir  string
	// base seeds the persisted config with fields to PRESERVE across pinning
	// (notably Host, when the agent had previously joined outbound). AgentID and
	// ServerPubKey are always overwritten from the pairing result.
	base     ClientConfig
	upgrader websocket.Upgrader
	throttle *pairThrottle

	// Token is the RAW 32-byte one-time reverse-pairing token this acceptor keys
	// the handshake tag with (design subproject F). A pairing succeeds only if the
	// dialing server presents a tag over the same token.
	//
	// It is armed via Arm (from the token the operator PASTES into the pairing web
	// under --pair-web, see pairing_web.go) and CLEARED on Disarm (including the
	// TTL auto-disarm), so a pasted T is never left in memory indefinitely. A
	// disarmed acceptor (nil Token) fails every dial closed. Read a per-attempt
	// COPY via begin() under the lock so an in-flight handshake never races Arm/
	// Disarm mutating this field.
	Token []byte

	// Now is injectable for tests; defaults to time.Now. (No longer consumed by
	// the token handshake, which derives all freshness from the exchanged nonces;
	// retained for the acceptor's own timing needs and API stability.)
	Now func() time.Time
	// Confirm is the CONSENT GATE invoked after a successful token handshake: the
	// acceptor calls Confirm(fingerprint) and pins + goes live ONLY if it returns
	// true. In the TOKEN model the operator's affirmative action is "paste T + arm"
	// (see pairing_web.go) — the passing tag verify IS the consent — so the wired
	// gate simply returns true, and the string passed here is the NON-LOAD-BEARING
	// server-key fingerprint (proto.ReversePairFingerprint) offered only for optional
	// operator eyeballing in the web status. The gate is kept as an injectable seam
	// so tests can auto-confirm (return true) or auto-decline (return false); it is
	// called with NO lock held (the in-flight slot is still claimed, so no
	// concurrent attempt races the decision). A nil Confirm FAILS CLOSED — treated
	// as a decline — so no pin is ever authorized without a gate.
	Confirm func(fingerprint string) bool
	// OnAttemptStart, if set, is invoked once a dial-in has upgraded and the token
	// handshake is ABOUT TO run — the "verifying" transition for the web status.
	// Called with no lock held.
	OnAttemptStart func()
	// OnAttemptError, if set, reports a FAILED token handshake (wrong/short token,
	// tampered transcript, dropped connection). Nothing is pinned and the acceptor
	// stays ARMED so the operator can retry within the token TTL. Called with no
	// lock held.
	OnAttemptError func(err error)
	// OnPaired is invoked EXACTLY ONCE, after the token verified and the config has
	// been pinned + persisted. cfg is the just-persisted config, serverPub the
	// pinned key, fingerprint the NON-LOAD-BEARING display fingerprint of the
	// server key. Called with no lock held, after the acceptor has disarmed. This
	// is where the cmd layer brings the R2 data listener live.
	OnPaired func(cfg ClientConfig, serverPub []byte, fingerprint string)
	// OnDeclined optionally reports that an otherwise-good handshake was DISCARDED
	// because the consent gate returned false (or was nil). Nothing was pinned and
	// the acceptor stays ARMED for a legitimate retry. fingerprint is the declined
	// pairing's display fingerprint. Called with no lock held.
	OnDeclined func(fingerprint string)
	// OnError optionally reports a persistence failure after a verified handshake
	// (the acceptor stays armed so the operator can retry).
	OnError func(err error)

	mu       sync.Mutex
	armed    bool
	inFlight bool
}

// NewPairingAcceptor builds an ARMED acceptor. identity supplies the agent's
// long-term keypair (its private key signs the pairing hello, its public key
// derives the AgentID pinned on success). dataDir is where agent_config.json is
// (re)written 0600. base supplies config fields to preserve (e.g. an existing
// Host); AgentID/ServerPubKey are set from the pairing result.
func NewPairingAcceptor(identity *Identity, dataDir string, base ClientConfig) *PairingAcceptor {
	return &PairingAcceptor{
		identity: identity,
		dataDir:  dataDir,
		base:     base,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: proto.PairingStepTimeout,
			ReadBufferSize:   4 << 10,
			WriteBufferSize:  4 << 10,
			// Machine-to-machine API; the token TAG (not an Origin header) is the
			// security boundary, so accept any origin and let the handshake decide.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		throttle: newPairThrottle(),
		Now:      time.Now,
		armed:    true,
	}
}

// Armed reports whether the acceptor is still accepting pairing (true until the
// first success or an explicit Disarm).
func (a *PairingAcceptor) Armed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed
}

// Arm sets the one-time reverse-pairing token and (re-)arms the acceptor. It is
// the agent-side hook the pairing web calls when the operator pastes T and
// clicks "arm" — that paste+arm is the operator's affirmative action. token must
// be the 32 raw bytes (the web canonicalizes+validates via
// proto.CanonicalizeReversePairToken first). A stored COPY is kept so the caller may
// reuse/zero its own slice; begin() hands each attempt its own copy in turn.
func (a *PairingAcceptor) Arm(token []byte) {
	tok := make([]byte, len(token))
	copy(tok, token)
	a.mu.Lock()
	a.Token = tok
	a.armed = true
	a.mu.Unlock()
}

// Disarm stops the acceptor accepting further pairing without a success (e.g. a
// time-window close, the token TTL expiring, or shutdown) and CLEARS the armed
// token from memory (design F Important #5 — never leave a pasted T armed
// indefinitely). Idempotent; safe under concurrency.
func (a *PairingAcceptor) Disarm() {
	a.mu.Lock()
	a.armed = false
	for i := range a.Token {
		a.Token[i] = 0
	}
	a.Token = nil
	a.mu.Unlock()
}

// begin atomically claims the single in-flight slot IFF the acceptor is armed
// and no attempt is already running, and returns a private COPY of the armed
// token for this attempt to key its handshake with. The copy is taken under the
// lock so an in-flight handshake never races Arm/Disarm mutating a.Token (Disarm
// zeroes the backing array). Returns ok=false to REJECT (disarmed, or a
// concurrent attempt already holds the slot).
func (a *PairingAcceptor) begin() (token []byte, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.armed || a.inFlight {
		return nil, false
	}
	a.inFlight = true
	tok := make([]byte, len(a.Token))
	copy(tok, a.Token)
	return tok, true
}

// finish releases the in-flight slot; on success it also DISARMS so no further
// pairing is ever accepted (first-contact surface, one shot).
func (a *PairingAcceptor) finish(success bool) {
	a.mu.Lock()
	a.inFlight = false
	if success {
		a.armed = false
	}
	a.mu.Unlock()
}

// ServeHTTP runs one bounded pairing attempt. Order matters: throttle →
// WS-only → arm+single-in-flight gate (BEFORE the upgrade, so rejected peers
// never even upgrade) → upgrade → token pairing handshake → consent gate →
// pin+persist → disarm. A handshake that fails the token verify pins NOTHING and
// brings NO channel live, and leaves the acceptor ARMED so a legitimate peer can
// still pair within the token TTL. On a passing verify the consent gate (wired to
// return true in the token flow, since paste+arm is the consent) authorizes the
// pin+go-live. Any handshake failure likewise leaves nothing pinned and (except a
// post-verify persistence error) keeps the acceptor armed for a retry.
func (a *PairingAcceptor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := pairClientIP(r)
	if a.throttle.blocked(ip) {
		http.Error(w, "too many pairing attempts", http.StatusTooManyRequests)
		return
	}
	// Exactly one thing is exposed here: a WebSocket upgrade.
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "expected a websocket upgrade", http.StatusBadRequest)
		return
	}
	// Arm + single-in-flight gate BEFORE upgrading. A disarmed acceptor (already
	// paired, not yet armed with a pasted token, or explicitly closed) and a
	// concurrent second attempt are both rejected here — the WS handshake never
	// even starts. begin() hands back a private copy of the armed token.
	token, ok := a.begin()
	if !ok {
		http.Error(w, "reverse pairing is not open", http.StatusConflict)
		return
	}

	conn, err := a.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		a.throttle.record(ip)
		a.finish(false)
		return
	}

	// A dial-in has upgraded and the token handshake is about to run — surface the
	// "verifying" transition to the web status.
	if a.OnAttemptStart != nil {
		a.OnAttemptStart()
	}

	wc := proto.NewWSPairConn(conn)
	// The token handshake VERIFIES the dialing server's tag over the shared token
	// (this attempt's private copy) BEFORE it returns serverPub to pin — a
	// wrong-token peer fails here and nothing is pinned. proto.RunAgentPairingHandshake
	// sends its own confirming tag ONLY after it has verified the server's tag.
	serverPub, err := proto.RunAgentPairingHandshake(wc, a.identity.PubKey, token)
	// Pairing is one-shot: the handshake is complete (or failed) and the
	// server-side dialer closes its end too, so close ours now — the consent gate
	// below must never depend on the socket staying open.
	_ = conn.Close()
	if err != nil {
		a.throttle.record(ip)
		a.finish(false)
		// A failed/dropped attempt pins NOTHING and leaves the acceptor ARMED so
		// the operator can retry within the token TTL (design F: single-use burns on
		// success-or-TTL, NOT on one failed attempt).
		if a.OnAttemptError != nil {
			a.OnAttemptError(err)
		}
		return
	}
	// NON-LOAD-BEARING display fingerprint of the pinned server key (the token tag
	// already authenticated it); shown to the operator for optional eyeballing.
	fp := proto.ReversePairFingerprint(serverPub)

	// CONSENT GATE. In the token flow the anti-MITM authentication is ALREADY done
	// (the tag verified above), and the operator's affirmative action was pasting T
	// + arming — so the wired gate returns true and pins. It is kept as an
	// injectable seam so tests can auto-decline; a nil Confirm fails CLOSED. We are
	// still holding the in-flight slot, so no concurrent attempt can race this
	// decision or a pin. The string passed is the NON-LOAD-BEARING fingerprint.
	confirm := a.Confirm
	if confirm == nil {
		confirm = func(string) bool { return false }
	}
	if !confirm(fp) {
		// DECLINED: DISCARD. Persist NOTHING, bring NO channel live, and STAY ARMED
		// so a legitimate peer can still pair — do NOT disarm on a declined pairing.
		a.finish(false)
		if a.OnDeclined != nil {
			a.OnDeclined(fp)
		}
		return
	}

	// CONFIRMED. PIN the server key and PERSIST — still holding the in-flight
	// slot, so no concurrent attempt can race the write. AgentID is the AGENT's
	// own fingerprint (mirrors the server-side proto.FingerprintPubKey(agentPub)); Host
	// and any other base fields are preserved.
	cfg := a.base
	cfg.AgentID = proto.FingerprintPubKey(a.identity.PubKey)
	cfg.ServerPubKey = base64.StdEncoding.EncodeToString(serverPub)
	if err := SaveClientConfig(a.dataDir, cfg); err != nil {
		// Do NOT disarm: the pairing was confirmed, only the local write failed —
		// let the operator retry rather than stranding an armed-but-unpinnable
		// agent.
		a.finish(false)
		if a.OnError != nil {
			a.OnError(err)
		}
		return
	}

	// Confirmed + pinned: DISARM (one shot) then hand the result up so the cmd
	// layer brings the data channel live with the freshly pinned key.
	a.finish(true)
	if a.OnPaired != nil {
		a.OnPaired(cfg, serverPub, fp)
	}
}

// pairClientIP extracts the peer IP for throttling (mirrors channel.clientIP).
func pairClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// The WebSocket-to-io.ReadWriter transport adapter this acceptor's handshake
// rides over now lives in proto.NewWSPairConn (internal/agent/proto/wsconn.go)
// — moved out of this file in the W3 proto-split (design analysis §6 W3, §7
// R1) so the SERVER-side dialer (internal/agentsrv/pairing_dial.go) can share
// the exact same adapter without a throwaway exported wrapper bolted onto this
// agent-only file.
