package proto

import "crypto/sha256"

// This file holds the SAS (Short Authentication String) derivation for
// remote-agent pairing (design W12-2b). A SAS is a short, human-comparable
// fingerprint of a security-critical value pair — the same trick Signal/
// ZRTP/Bluetooth pairing use: instead of asking a human to compare two raw
// hex fingerprints, both sides render the SAME deterministic hash as a
// short sequence of easy-to-recognize, hard-to-confuse symbols (here,
// emoji) and the human eyeballs a match.
//
// THE MITM PROPERTY THIS BUYS, PRECISELY:
//
// DeriveSAS binds BOTH the pairing code (a shared secret the admin handed
// the agent operator out of band) AND the agent's ed25519 public key. The
// server computes and stores its SAS from whatever pubkey it actually
// RECEIVED in the pair/complete request (server/handles/agent_pair.go).
// The agent — critically — must compute ITS OWN copy of the SAS purely
// LOCALLY, from the pairing code the operator typed in plus the identity
// it generated itself (cmd/agent.go's `agent join`), and display THAT, not
// whatever a server response echoes back. A network-position attacker who
// substitutes the agent's real public key for one they control in transit
// changes what the server received (and therefore what its stored SAS
// encodes) without being able to change what the agent computed locally
// from its own real key — so the two independently-rendered emoji
// sequences (one on the admin's confirm page, one on the agent's own
// terminal) diverge, and a human comparing them face to face / by phone
// (a channel the attacker does not control) catches it. A passive,
// non-tampering relay changes nothing about either input, so the two sides
// always agree — the SAS never produces a false mismatch.
//
// The pairing code itself is what ties a specific SAS rendering to a
// specific pairing attempt (without it, two concurrent pairings could
// collide in the emoji they render purely by low sequence length); the
// code is single-use and already validated before DeriveSAS is ever
// called, so this is not doing double duty as the actual authentication —
// it is purely what makes the SAS meaningful to compare "for this
// pairing".

// sasAlphabet is the curated set of emoji DeriveSAS renders a SAS from.
// Every entry is a single, unambiguous, universally-rendered emoji
// (Unicode Supplemental Symbols and Pictographs block and later — codepoint
// >= U+1F300 — so every one of them is emoji-presentation BY DEFAULT and
// never needs a variation selector to avoid falling back to a text glyph).
// Deliberately chosen for visual distinctiveness (no near-duplicate faces/
// shapes) so a human can tell two sequences apart at a glance. The length
// (64, a power of two) is load-bearing for DeriveSAS's index math: 256 (a
// byte's range) divides evenly by 64, so mapping a hash byte to an index via
// modulo introduces NO bias — every emoji is equally likely.
var sasAlphabet = []string{
	"🍎", "🍌", "🍇", "🍉", "🍓", "🍒", "🍑", "🥝", "🍍", "🥥",
	"🌽", "🥕", "🍄", "🌰", "🐶", "🐱", "🐭", "🐹", "🐰", "🦊",
	"🐻", "🐼", "🐨", "🐯", "🦁", "🐮", "🐷", "🐸", "🐵", "🐔",
	"🐧", "🐦", "🦆", "🦉", "🦋", "🐢", "🐍", "🐳", "🐬", "🐟",
	"🐙", "🦀", "🐝", "🐌", "🐞", "🦄", "🐴", "🦓", "🐘", "🐫",
	"🦘", "🦥", "🦦", "🐺", "🦌", "🐊", "🦎", "🐡", "🦈", "🐋",
	"🦭", "🦩", "🦚", "🦜",
}

// sasLength is how many emoji DeriveSAS renders. 5 symbols out of a
// 64-alphabet is 30 bits of rendered entropy (log2(64^5)) — comfortably
// enough that an accidental collision between two unrelated pairings is not
// a practical concern for what the SAS defends (a human visually comparing
// two short sequences, not a cryptographic commitment on its own), while
// staying short enough to actually compare at a glance.
const sasLength = 5

// DeriveSAS computes the SAS for one pairing attempt: h =
// sha256(code || pubKey), then maps the first sasLength bytes of h to
// sasAlphabet entries (h[i] % 64, unbiased — see sasAlphabet's doc). Pure
// and deterministic: given the same (code, pubKey), every caller — the
// server and the agent alike — always computes the exact same sequence.
// See this file's top-of-file doc for exactly what security property that
// buys and why both code and pubKey must be bound into it.
//
// INTENTIONAL DOMAIN ASYMMETRY (do not "fix"): unlike the reverse SAS
// (DeriveReverseSAS, which prepends reverseSASDomain), this outbound SAS
// prepends NO domain tag — it hashes code‖pubKey directly. Adding a tag here
// for hygiene is tempting but would change this hash's preimage and therefore
// the rendered SAS value, which every already-paired outbound agent computes
// LOCALLY (cmd/agent.go `agent join`) and re-derives on re-pair/confirm. That
// would break confirmation for the existing outbound fleet. The domain
// separation the reverse SAS needs is fully provided by ITS tag alone (a
// tagged hash can never collide with an untagged one), so the two schemes
// stay distinct without touching this preimage. The asymmetry is acceptable.
func DeriveSAS(code string, pubKey []byte) []string {
	h := sha256.Sum256(append([]byte(code), pubKey...))
	out := make([]string, sasLength)
	for i := 0; i < sasLength; i++ {
		out[i] = sasAlphabet[int(h[i])%len(sasAlphabet)]
	}
	return out
}
