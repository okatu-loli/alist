package common

import (
	"time"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/pkg/errors"
)

var SecretKey []byte

// Refresh-token lifetime is expressed as a multiple of the access-token
// lifetime (conf.Conf.TokenExpiresIn hours), so a short-lived access token
// can be silently renewed via /api/auth/refresh without forcing a full
// re-login. RefreshTokenRememberFactor further stretches that when the
// client asked to be remembered (e.g. a "remember me" login).
const (
	RefreshTokenFactor         = 7
	RefreshTokenRememberFactor = 4
)

type UserClaims struct {
	Username  string `json:"username"`
	PwdTS     int64  `json:"pwd_ts"`
	JTI       string `json:"jti"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// generateSignedToken signs a new JWT of the given kind for user, bound to
// sessionID (the device session key), and records its jti in tokenstore so
// validity can be checked from persistent state instead of process memory.
// Shared by GenerateToken and GenerateRefreshToken — the two differ only in
// Kind and ttl.
func generateSignedToken(user *model.User, sessionID, kind string, ttl time.Duration) (tokenString string, err error) {
	jti := uuid.NewString()
	expiresAt := time.Now().Add(ttl)
	claim := UserClaims{
		Username:  user.Username,
		PwdTS:     user.PwdTS,
		JTI:       jti,
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		}}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claim)
	tokenString, err = token.SignedString(SecretKey)
	if err != nil {
		return "", err
	}
	store := tokenstore.Get()
	if store == nil {
		return "", errors.New("token store is not initialized")
	}
	if err := store.Save(tokenstore.Record{
		JTI:       jti,
		UserID:    user.ID,
		SessionID: sessionID,
		Kind:      kind,
		ExpiresAt: expiresAt.Unix(),
	}); err != nil {
		return "", err
	}
	return tokenString, nil
}

// GenerateToken signs a new access token for user, bound to sessionID (the
// device session key). The jti is recorded in tokenstore so validity can be
// checked from persistent state instead of process memory.
func GenerateToken(user *model.User, sessionID string) (tokenString string, err error) {
	ttl := time.Duration(conf.Conf.TokenExpiresIn) * time.Hour
	return generateSignedToken(user, sessionID, model.TokenKindAccess, ttl)
}

// GenerateRefreshToken signs a new refresh token for user, bound to
// sessionID. Its lifetime is a multiple of the access token's
// (RefreshTokenFactor), further stretched by RefreshTokenRememberFactor when
// remember is true. Refresh tokens are exchanged at /api/auth/refresh for a
// new access+refresh pair; see server/handles.Refresh for the rotation and
// replay-detection logic that consumes the Kind recorded here.
func GenerateRefreshToken(user *model.User, sessionID string, remember bool) (tokenString string, err error) {
	factor := RefreshTokenFactor
	if remember {
		factor *= RefreshTokenRememberFactor
	}
	ttl := time.Duration(conf.Conf.TokenExpiresIn*factor) * time.Hour
	return generateSignedToken(user, sessionID, model.TokenKindRefresh, ttl)
}

// ParseTokenClaims verifies only the JWT signature and standard claims
// (expiry/not-before) and extracts UserClaims — it does not consult
// tokenstore. Most callers want ParseToken instead, which additionally
// fail-closes against the persistent store. ParseTokenClaims exists for
// server/handles.Refresh, which needs the store lookup's error kept apart
// (revoked vs. merely unknown/expired) to detect refresh-token replay,
// rather than collapsed into ParseToken's single opaque error.
func ParseTokenClaims(tokenString string) (*UserClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &UserClaims{}, func(token *jwt.Token) (interface{}, error) {
		return SecretKey, nil
	})
	if err != nil {
		if ve, ok := err.(*jwt.ValidationError); ok {
			if ve.Errors&jwt.ValidationErrorMalformed != 0 {
				return nil, errors.New("that's not even a token")
			} else if ve.Errors&jwt.ValidationErrorExpired != 0 {
				return nil, errors.New("token is expired")
			} else if ve.Errors&jwt.ValidationErrorNotValidYet != 0 {
				return nil, errors.New("token not active yet")
			} else {
				return nil, errors.New("couldn't handle this token")
			}
		}
	}
	claims, ok := token.Claims.(*UserClaims)
	if !ok || !token.Valid {
		return nil, errors.New("couldn't handle this token")
	}
	return claims, nil
}

func ParseToken(tokenString string) (*UserClaims, error) {
	claims, err := ParseTokenClaims(tokenString)
	if err != nil {
		return nil, err
	}
	// The JWT signature alone is not sufficient: the jti must still be known,
	// unrevoked and unexpired in the persistent store. A nil store (Init not
	// yet called) is treated as "reject everything" rather than panicking.
	store := tokenstore.Get()
	if store == nil {
		return nil, errors.New("token store is not initialized")
	}
	rec, err := store.Get(claims.JTI)
	if err != nil {
		return nil, errors.New("token is invalidated")
	}
	// A refresh token is structurally indistinguishable from an access token
	// (same claims, same signature scheme) and its jti is equally present in
	// the store, so without this check it would pass as a bearer token for
	// its whole — much longer — lifetime, bypassing the rotation/replay
	// detection that /api/auth/refresh relies on entirely.
	if rec.Kind != model.TokenKindAccess {
		return nil, errors.New("token is invalidated")
	}
	return claims, nil
}

// InvalidateToken revokes tokenString's jti in the persistent store so it is
// rejected by ParseToken even before its natural expiry.
func InvalidateToken(tokenString string) error {
	if tokenString == "" {
		return nil // don't invalidate empty guest token
	}
	token, _, err := jwt.NewParser().ParseUnverified(tokenString, &UserClaims{})
	if err != nil {
		return nil
	}
	claims, ok := token.Claims.(*UserClaims)
	if !ok || claims.JTI == "" {
		return nil
	}
	store := tokenstore.Get()
	if store == nil {
		return errors.New("token store is not initialized")
	}
	return store.Revoke(claims.JTI)
}
