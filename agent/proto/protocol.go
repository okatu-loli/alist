package proto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// FingerprintPubKey derives an agent's stable AgentID from its long-term
// ed25519 public key: the first 16 bytes (128 bits) of SHA-256(pubkey),
// hex-encoded (32 hex chars). Both sides of the pairing protocol —
// server/handles/agent_pair.go when a pairing completes, and this package
// when an identity wants to know its own eventual id — call this exact
// function, so the two can never diverge on how an id is derived from a key.
// 128 bits is far beyond accidental-collision range for the number of
// agents any single alist instance will ever pair.
func FingerprintPubKey(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:16])
}

// HeartbeatMessage builds the exact byte sequence an agent signs (with its
// long-term ed25519 private key) and the server verifies (against the
// stored public key) on every heartbeat: agentID, unix ts and a per-request
// nonce, "|"-joined. Binding all three into one signed message is what
// makes the heartbeat a genuine challenge-response proof rather than a
// static bearer token: ts anchors it to a narrow time window (see the
// server's freshness check) and nonce lets the server refuse a byte-for-
// byte repeat within that window (see the server's seen-nonce cache) — a
// captured heartbeat cannot be replayed even a second later.
//
// Both the client (internal/agent's Heartbeat) and the server
// (server/handles/agent_pair.go's AgentHeartbeat) MUST build this message
// through this one function — any divergence in framing between the two
// sides would make every signature fail to verify.
func HeartbeatMessage(agentID string, ts int64, nonce string) []byte {
	return []byte(agentID + "|" + strconv.FormatInt(ts, 10) + "|" + nonce)
}
