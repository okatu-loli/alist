package agent

import "testing"

// randString must stay drop-in compatible with pkg/utils/random.String: same
// 62-symbol alphabet, same crypto/rand entropy source, same "n chars" length
// contract. This is the only thing client.go / channel_capclient.go need from
// that package, so replacing the import with this local helper is what lets
// internal/agent drop its last non-channel main-repo dependency (W2).
const randStringAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func TestRandStringLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 62, 100} {
		s := randString(n)
		if len(s) != n {
			t.Errorf("randString(%d): got length %d, want %d", n, len(s), n)
		}
	}
}

func TestRandStringAlphabet(t *testing.T) {
	s := randString(256)
	for i, r := range s {
		if !containsRune(randStringAlphabet, r) {
			t.Fatalf("randString: byte %d (%q) not in expected 62-char alphabet", i, r)
		}
	}
}

// TestRandStringVaries is a weak, probabilistic non-collision check: with a
// 62^16 keyspace, 32 independent 16-char draws colliding would indicate a
// broken (e.g. all-zero) random source, not bad luck.
func TestRandStringVaries(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		s := randString(16)
		if seen[s] {
			t.Fatalf("randString(16): got duplicate value %q across %d draws", s, i+1)
		}
		seen[s] = true
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
