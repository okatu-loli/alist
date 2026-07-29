package proto

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// This file holds the REVERSE-pairing SAS derivation (design subproject E,
// wave E1). It is the first-contact / TOFU counterpart to DeriveSAS (sas.go):
// where the outbound SAS binds a shared pairing CODE plus the agent's pubkey,
// reverse pairing has NO code (the server dials the agent's serve address, so
// there is no out-of-band secret handed over first). Instead the reverse SAS
// binds the FULL first-contact transcript both sides can independently see:
// BOTH freshly-exchanged long-term ed25519 public keys AND BOTH sides' fresh
// per-handshake nonces.
//
// WHY THOSE FOUR INPUTS — the exact MITM property this buys:
//
//   - Binding BOTH pubkeys is the anti-MITM core. Neither side has a pinned
//     key at first contact (that is the whole point — TOFU), so the ONLY
//     defense against a network-position attacker substituting a key in flight
//     is that each side derives the SAS from the key IT locally believes is the
//     peer's, and a human compares the two renderings out of band. If a MITM
//     swaps the agent's real key Y for its own X, the server derives from X and
//     the agent derives from its own Y → the two emoji sequences DIVERGE → the
//     human comparing them (over a channel the attacker does not control)
//     catches it. Symmetrically for the server key. A passive, non-tampering
//     relay changes neither key, so honest runs always agree — no false
//     mismatch.
//   - Binding BOTH nonces makes the rendered SAS fresh to THIS exact exchange:
//     it cannot be pre-computed before the handshake (anti-precompute) and a
//     captured transcript cannot be replayed to reproduce a previously-eyeballed
//     SAS for a different exchange (anti-replay). The nonces are 32 bytes from
//     crypto/rand, one per side per handshake.
//
// Domain separation: the hash is prefixed with reverseSASDomain, a constant
// distinct from anything DeriveSAS hashes (DeriveSAS prepends no domain tag at
// all — it hashes code‖pubKey directly), so a reverse SAS and an outbound SAS
// can never collide even on deliberately overlapping byte inputs. Rendering
// reuses the SAME sasAlphabet and the SAME sasLength (5 emoji) as DeriveSAS, so
// a human compares an identically-shaped sequence in both pairing modes.

// reverseSASDomain is the domain-separation tag prepended to the reverse-SAS
// hash input. The "-v1" suffix reserves room to rotate the derivation later
// without silently colliding with values rendered under the current scheme.
const reverseSASDomain = "agent-reverse-pair-sas-v1"

// reverseSASInputBytes is the exact length each of DeriveReverseSAS's four
// inputs must have: an ed25519 public key and a pairing nonce are both 32
// bytes, so a single constant guards all four.
const reverseSASInputBytes = 32

// DeriveReverseSAS computes the reverse-pairing SAS from the first-contact
// transcript:
//
//	h = sha256( reverseSASDomain ‖ serverPub ‖ agentPub ‖ nonceS ‖ nonceA )
//
// then maps the first sasLength bytes of h to sasAlphabet entries (h[i] % 64,
// unbiased — see sasAlphabet's doc in sas.go), joined by a single space (the
// same on-the-wire/on-disk rendering model/agent.go's PendingSAS uses). Pure
// and deterministic: given the same four inputs, the server and the agent —
// each deriving purely from what IT locally holds — always compute the exact
// same sequence. See the top-of-file doc for exactly why all four inputs are
// bound and what MITM property that buys.
//
// DEFENSIVE LENGTH GUARD: both runners already validate every input to exactly
// 32 bytes (ed25519.PublicKeySize for the keys, pairingNonceBytes for the
// nonces) via decodePairingPubKey/decodePairingNonce BEFORE calling this, so
// on the real paths the error is unreachable. It exists purely so a future or
// misusing caller cannot feed a short/attacker-truncatable slice and still get
// a rendered SAS out — a truncated input would silently weaken the binding, so
// we refuse rather than render. Returning an error (not a sentinel string)
// forces that caller to handle the misuse instead of comparing a fixed value.
func DeriveReverseSAS(serverPub, agentPub, nonceS, nonceA []byte) (string, error) {
	for _, in := range [][]byte{serverPub, agentPub, nonceS, nonceA} {
		if len(in) != reverseSASInputBytes {
			return "", fmt.Errorf("agentpair: DeriveReverseSAS requires %d-byte inputs (got %d)", reverseSASInputBytes, len(in))
		}
	}
	h := sha256.New()
	h.Write([]byte(reverseSASDomain))
	h.Write(serverPub)
	h.Write(agentPub)
	h.Write(nonceS)
	h.Write(nonceA)
	sum := h.Sum(nil)

	out := make([]string, sasLength)
	for i := 0; i < sasLength; i++ {
		out[i] = sasAlphabet[int(sum[i])%len(sasAlphabet)]
	}
	return strings.Join(out, " "), nil
}
