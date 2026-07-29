package agent

import (
	"crypto/rand"
	"math/big"
)

// randStringAlphabet62 is the exact 62-symbol alphabet pkg/utils/random.String
// uses. Keeping it byte-for-byte identical matters: client.go's heartbeat
// nonce doc (see heartbeatNonceLen) and streamChunkIDLen's entropy budget are
// both reasoned about in terms of "62-symbol alphabet", not this package's
// own choice.
const randStringAlphabet62 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randString returns n characters drawn uniformly (via crypto/rand, one
// rejection-sampled draw per character — see math/big.Int backed
// crypto/rand.Int) from randStringAlphabet62. It is a local, dependency-free
// stand-in for pkg/utils/random.String(n): same alphabet, same entropy
// source, same "n characters out" contract, kept unexported since nothing
// outside this package (nor, per design, outside the eventual alist-agent
// module) needs to call it. It cannot just import pkg/utils/random.String
// instead: pkg/utils lives in the core alist module, and this module's
// boundary rule (see ../doc.go) forbids importing anything from core.
func randString(n int) string {
	b := make([]byte, n)
	alphabetLen := big.NewInt(int64(len(randStringAlphabet62)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			panic(err)
		}
		b[i] = randStringAlphabet62[idx.Int64()]
	}
	return string(b)
}
