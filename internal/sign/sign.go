package sign

import (
	"errors"
	"sync"
	"time"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
	pkgsign "github.com/alist-org/alist/v3/pkg/sign"
	log "github.com/sirupsen/logrus"
)

// signReloadMinInterval throttles the unknown-kid reload-and-retry in
// Verify: at most one Reload() per interval, no matter how many verifies
// with an unknown kid arrive in that window. Without this, a burst of
// requests carrying a stale/forged kid would each trigger a db read.
const signReloadMinInterval = 10 * time.Second

var (
	reloadMu   sync.Mutex
	lastReload time.Time
)

// tryReloadForUnknownKid reloads the download signer, throttled to at most
// once per signReloadMinInterval, and reports whether an attempt was made
// (regardless of whether Reload itself succeeded — Verify only needs to
// know whether re-verifying against a possibly-refreshed signer is worth
// it). This is what lets a live process pick up a rotation or GC performed
// by another process (e.g. the CLI) without a restart: the kid it doesn't
// recognize was likely just added to the db out-of-band.
func tryReloadForUnknownKid() bool {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	if time.Since(lastReload) < signReloadMinInterval {
		return false
	}
	lastReload = time.Now()
	if err := Reload(); err != nil {
		log.Errorf("sign: reload on unknown kid failed: %s", err.Error())
	}
	return true
}

func Sign(data string) string {
	expire := setting.GetInt(conf.LinkExpiration, 0)
	if expire == 0 {
		return NotExpired(data)
	} else {
		return WithDuration(data, time.Duration(expire)*time.Hour)
	}
}

func WithDuration(data string, d time.Duration) string {
	s := get()
	if s == nil {
		log.Errorf("sign: no download signer available, cannot sign %q", data)
		return ""
	}
	return s.Sign(data, time.Now().Add(d).Unix())
}

func NotExpired(data string) string {
	s := get()
	if s == nil {
		log.Errorf("sign: no download signer available, cannot sign %q", data)
		return ""
	}
	return s.Sign(data, 0)
}

func Verify(data string, sign string) error {
	s := get()
	if s == nil {
		log.Errorf("sign: no download signer available, cannot verify %q", data)
		return pkgsign.ErrSignerUnavailable
	}
	err := s.Verify(data, sign)
	if errors.Is(err, pkgsign.ErrKeyUnknown) && tryReloadForUnknownKid() {
		if s2 := get(); s2 != nil {
			err = s2.Verify(data, sign)
		}
	}
	return err
}
