package agentsrv

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"

	"github.com/AlistGo/alist-agent/proto"
)

// This file is the SERVER (dialer) side of TOKEN-AUTHENTICATED first-contact
// reverse pairing (design subproject F). It is the one-shot counterpart to the
// agent acceptor (internal/agent/pairing_acceptor.go): the server, which HAS a
// reachable route to the agent's public serve listener, DIALS the agent's
// /agentpair endpoint, runs the server pairing leg (proto.
// RunServerPairingHandshake) over the WS keyed with the shared one-time token,
// and returns the agent's freshly-learned long-term public key + derived
// agentID + a DISPLAY fingerprint of the server key. It then CLOSES the
// socket: pairing is single-shot and opens NO persistent channel here — the
// reverse supervisor dials the real pinned-key data channel later.
//
// WHY IT LIVES IN PACKAGE agentsrv (import-direction decision, updated by the
// W1 server/agent repo split): this is SERVER-only code (it dials OUT from the
// AList server), so it lives next to the rest of the server-side agent state
// (taskcap.go, pairing.go, serveridentity.go) rather than in internal/agent,
// which package cmd/agent's binary also loads. The handshake it drives
// (proto.RunServerPairingHandshake) and the WS transport adapter
// (proto.NewWSPairConn) live in internal/agent/proto — the shared wire-protocol
// package both a server and an agent process import (design analysis §6 W3) —
// so this file imports proto, never internal/agent itself: package proto must
// never import agentsrv (or internal/agent), or the protocol layer would gain
// a runtime-side dependency and reopen the import cycle the split removes.
//
// 🔒 SECURITY: DialAndPairAgent triggers an OUTBOUND dial from the SERVER to an
// admin-supplied URL, admin-gated and admin-typed. The token tag is the anti-
// MITM backstop: a non-agent endpoint fails the handshake and no row is created;
// a wrong-token peer fails the tag verify. The dial+handshake is bounded
// (pairingDialTimeout + the per-step wsPairConn deadlines).
//
// TODO(F3): the token is currently a REQUIRED input handed in by the caller. F3
// owns the server-side token LIFECYCLE (mint via GenerateReversePairToken,
// display to the operator, in-memory single-use store with a ≤10min TTL) and
// passes the raw bytes here.

// pairingDialTimeout bounds ONLY the DialContext (the TCP/WS connect), NOT the
// whole handshake: the pairing handshake that follows is bounded SEPARATELY by
// the per-step wsPairConn deadlines (pairingStepTimeout on each read/write). So
// an unreachable endpoint cannot wedge the connect, and a connected-but-stalling
// endpoint cannot wedge any single handshake step.
const pairingDialTimeout = 20 * time.Second

// DialAndPairAgent dials the agent's first-contact pairing endpoint at wsURL
// (e.g. wss://vps:5343/agentpair), runs the SERVER leg of the token pairing
// handshake (presenting serverPub, keyed with tokenRaw), and returns the agent's
// presented long-term public key, its derived agentID (FingerprintPubKey), and a
// NON-LOAD-BEARING display fingerprint of the server key (ReversePairFingerprint
// — offered for optional operator eyeballing; the token tag is the actual auth).
// On any dial/handshake/verify failure it closes the connection and returns the
// error — nothing is persisted, no channel goes live. The connection is ALWAYS
// closed before return: this is one-shot pairing, not a persistent channel.
func DialAndPairAgent(ctx context.Context, wsURL string, serverPub, tokenRaw []byte) (agentPub []byte, agentID string, fingerprint string, err error) {
	if len(serverPub) != ed25519.PublicKeySize {
		return nil, "", "", errors.New("agentpair: server public key is missing/malformed")
	}
	if err := proto.ValidatePairingToken(tokenRaw); err != nil {
		return nil, "", "", err
	}

	dialCtx, cancel := context.WithTimeout(ctx, pairingDialTimeout)
	defer cancel()

	conn, _, derr := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if derr != nil {
		return nil, "", "", fmt.Errorf("agentpair: dial %s: %w", wsURL, derr)
	}
	// One-shot: the pairing conn is always closed. No persistent channel is held
	// here — the reverse supervisor opens the pinned-key data channel later.
	defer func() { _ = conn.Close() }()

	wc := proto.NewWSPairConn(conn)
	agentPub, agentID, err = proto.RunServerPairingHandshake(wc, serverPub, tokenRaw)
	if err != nil {
		return nil, "", "", err
	}
	return agentPub, agentID, proto.ReversePairFingerprint(serverPub), nil
}
