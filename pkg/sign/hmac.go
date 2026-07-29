package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"time"
)

// KeyResolver looks up the secret for a given kid. Used by Verify so a
// signature minted under a grace (rotated-out) key still validates as long
// as the resolver still knows about it.
type KeyResolver func(kid string) ([]byte, bool)

type HMACSign struct {
	SecretKey []byte      // active key, used for Sign
	KID       string      // active kid, embedded in output
	Resolve   KeyResolver // used by Verify to look up any (active or grace) key
}

func (s HMACSign) Sign(data string, expire int64) string {
	return computeHMAC(s.SecretKey, s.KID, data, expire)
}

func computeHMAC(secret []byte, kid string, data string, expire int64) string {
	h := hmac.New(sha256.New, secret)
	expireTimeStamp := strconv.FormatInt(expire, 10)
	_, err := io.WriteString(h, data+":"+expireTimeStamp+":"+kid)
	if err != nil {
		return ""
	}

	return base64.URLEncoding.EncodeToString(h.Sum(nil)) + ":" + expireTimeStamp + ":" + kid
}

func (s HMACSign) Verify(data, sign string) error {
	signSlice := strings.Split(sign, ":")
	// kid is the last colon segment, expire the second-last; both are colon-free, so a right-anchored split is unambiguous even when data itself contains ':'
	if len(signSlice) < 3 {
		return ErrSignInvalid
	}
	kid := signSlice[len(signSlice)-1]
	expireStr := signSlice[len(signSlice)-2]
	// check whether contains expire time
	if expireStr == "" {
		return ErrExpireMissing
	}
	// check whether expire time is expired
	expires, err := strconv.ParseInt(expireStr, 10, 64)
	if err != nil {
		return ErrExpireInvalid
	}
	// if expire time is expired, return error
	if expires < time.Now().Unix() && expires != 0 {
		return ErrSignExpired
	}
	secret, ok := s.Resolve(kid)
	if !ok {
		return ErrKeyUnknown
	}
	// verify sign
	if computeHMAC(secret, kid, data, expires) != sign {
		return ErrSignInvalid
	}
	return nil
}

func NewHMACSign(secret []byte, kid string, resolve KeyResolver) Sign {
	return HMACSign{SecretKey: secret, KID: kid, Resolve: resolve}
}
