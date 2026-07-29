package sign

import (
	"sync"

	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/sign"
	log "github.com/sirupsen/logrus"
)

// downloadSigner is rebuilt from the signing_keys table (purpose "download")
// on first use and whenever Reload is called (e.g. after rotation). Every
// access goes through the RWMutex since Reload can race Sign/Verify.
var (
	mu             sync.RWMutex
	downloadSigner sign.Sign
)

// buildDownloadSigner reads the active "download" key as the signer's
// signing key, and every "download" key (active or grace) as the verify
// resolver, so a signature minted under a since-rotated-out key still
// verifies until an admin deletes it.
//
// Secret encoding: model.SigningKey.Secret holds base64(32 random bytes) as
// produced by the seeder/rotation code. The HMAC secret used here is the raw
// bytes of that base64 *string* ([]byte(k.Secret)), not the decoded bytes —
// sign and verify both read Secret through this same conversion, so the
// on-disk encoding is opaque to HMAC correctness as long as it stays
// consistent.
func buildDownloadSigner() (sign.Sign, error) {
	active, err := db.GetActiveSigningKey(model.SignPurposeDownload)
	if err != nil {
		return nil, err
	}
	keys, err := db.GetSigningKeys(model.SignPurposeDownload)
	if err != nil {
		return nil, err
	}
	secrets := make(map[string][]byte, len(keys))
	for _, k := range keys {
		secrets[k.KID] = []byte(k.Secret)
	}
	resolve := func(kid string) ([]byte, bool) {
		s, ok := secrets[kid]
		return s, ok
	}
	return sign.NewHMACSign([]byte(active.Secret), active.KID, resolve), nil
}

// Reload rebuilds the download signer from the current db state. Called
// lazily on first use (via get), explicitly after key rotation, and by
// Verify (throttled, see sign.go) when a verify misses on an unknown kid —
// that last path is what lets a live server pick up a rotation/GC done by
// another process (e.g. the CLI) without a restart.
//
// On build failure the previous signer is kept in place (a transient db
// hiccup must not blow away a working signer); the error is returned so
// callers that need a fresh signer to actually be usable (Rotate, GC) can
// detect and report the failure.
func Reload() error {
	s, err := buildDownloadSigner()
	if err != nil {
		log.Errorf("failed to build download signer: %s", err.Error())
		return err
	}
	mu.Lock()
	downloadSigner = s
	mu.Unlock()
	return nil
}

func get() sign.Sign {
	mu.RLock()
	s := downloadSigner
	mu.RUnlock()
	if s != nil {
		return s
	}
	_ = Reload()
	mu.RLock()
	defer mu.RUnlock()
	return downloadSigner
}
