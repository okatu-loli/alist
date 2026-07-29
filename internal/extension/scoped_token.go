package extension

import (
	"fmt"
	"time"

	"github.com/alist-org/alist/v3/internal/auth/tokenstore"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/pkg/errors"
)

// MaxScopedTokenTTL caps a scoped token's lifetime. A scoped token hands an
// extension direct storage reach, so it is deliberately short-lived (SDK §3:
// "short-lived receipt"); requests for longer are clamped, not rejected.
const MaxScopedTokenTTL = 15 * time.Minute

// Scope is one path grant requested for a scoped token. Write flips the grant
// from read-only (the default) to read-write.
type Scope struct {
	Path  string `json:"path"`
	Write bool   `json:"write,omitempty"`
}

// scopedClaims is the JWT payload of a scoped token. Paths and Write are
// parallel slices: Paths[i] is granted Write[i]. The jti (RegisteredClaims.ID)
// is recorded in tokenstore so the token is revocable and expiry-swept by the
// same GC as access tokens.
type scopedClaims struct {
	ExtID string   `json:"ext"`
	Paths []string `json:"paths"`
	Write []bool   `json:"write,omitempty"`
	jwt.RegisteredClaims
}

// scopedSecret is the HMAC secret scoped tokens are signed with — the same JWT
// secret server/common uses for access tokens (common.SecretKey =
// []byte(conf.Conf.JwtSecret)), read straight from conf to avoid depending on
// the server package from internal/.
func scopedSecret() ([]byte, error) {
	if conf.Conf == nil {
		return nil, errors.New("extension: conf not initialized")
	}
	return []byte(conf.Conf.JwtSecret), nil
}

// IssueScopedToken mints a short-lived token restricted to the requested
// scopes. Every requested path MUST be equal to or under one of the
// extension's manifest-declared Permissions.StorageTokens paths; an over-scope
// request is rejected outright. ttl is clamped to MaxScopedTokenTTL. The jti is
// saved in tokenstore, so the token can be revoked before expiry.
//
// StorageTokens is interpreted here as a list of path prefixes. The SDK's
// "self"/provider-id semantics are reconciled when the embedded auth broker +
// storage-token binding land (Step 2b); until then a literal path declaration
// is what grants a scope. Seam.
func IssueScopedToken(r *Registry, extID string, scopes []Scope, ttl time.Duration) (string, error) {
	e, ok := r.Get(extID)
	if !ok || !e.Enabled() {
		return "", fmt.Errorf("extension %q: not enabled", extID)
	}
	declared := e.Manifest.Permissions.StorageTokens
	for _, s := range scopes {
		if !scopeDeclared(declared, s.Path) {
			return "", fmt.Errorf("extension %q: scope %q exceeds declared storage_tokens", extID, s.Path)
		}
	}
	if ttl <= 0 || ttl > MaxScopedTokenTTL {
		ttl = MaxScopedTokenTTL
	}
	secret, err := scopedSecret()
	if err != nil {
		return "", err
	}
	store := tokenstore.Get()
	if store == nil {
		return "", errors.New("extension: token store is not initialized")
	}

	jti := uuid.NewString()
	expiresAt := time.Now().Add(ttl)
	paths := make([]string, len(scopes))
	writes := make([]bool, len(scopes))
	for i, s := range scopes {
		paths[i] = utils.FixAndCleanPath(s.Path)
		writes[i] = s.Write
	}
	claims := scopedClaims{
		ExtID: extID,
		Paths: paths,
		Write: writes,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}
	tokenString, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
	if err != nil {
		return "", err
	}
	if err := store.Save(tokenstore.Record{
		JTI:       jti,
		Kind:      model.TokenKindExtScoped,
		ExpiresAt: expiresAt.Unix(),
	}); err != nil {
		return "", err
	}
	return tokenString, nil
}

// AuthenticateScopedToken verifies a scoped token and builds the synthetic
// restricted principal it authenticates as. Verification is fail-closed and
// mirrors common.ParseToken: signature + standard claims, then the jti must
// still be active in tokenstore (unknown/revoked/expired all reject), and the
// stored kind must be the scoped kind — an access/refresh jti presented here is
// rejected, and a scoped jti is rejected by common.ParseToken as a bearer.
func AuthenticateScopedToken(tokenString string) (*model.User, error) {
	secret, err := scopedSecret()
	if err != nil {
		return nil, err
	}
	var claims scopedClaims
	token, err := jwt.ParseWithClaims(tokenString, &claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return secret, nil
	})
	if err != nil || !token.Valid {
		return nil, errors.New("invalid scoped token")
	}
	store := tokenstore.Get()
	if store == nil {
		return nil, errors.New("extension: token store is not initialized")
	}
	rec, err := store.Get(claims.ID)
	if err != nil {
		return nil, errors.New("scoped token is invalidated")
	}
	if rec.Kind != model.TokenKindExtScoped {
		return nil, errors.New("not a scoped token")
	}
	return scopedPrincipal(claims.ExtID, claims.Paths, claims.Write), nil
}

// IsScopedToken reports whether tokenString is shaped like a scoped token (it
// carries a non-empty "ext" claim), WITHOUT verifying its signature or store
// state. It is a cheap discriminator so the auth middleware can route only
// scoped-shaped tokens through AuthenticateScopedToken and leave normal bearer
// tokens on their existing path (avoiding a redundant token-store hit per
// request). Authentication is still fail-closed in AuthenticateScopedToken.
func IsScopedToken(tokenString string) bool {
	if tokenString == "" {
		return false
	}
	var claims scopedClaims
	if _, _, err := jwt.NewParser().ParseUnverified(tokenString, &claims); err != nil {
		return false
	}
	return claims.ExtID != ""
}

// RevokeScopedToken revokes a scoped token by its jti so AuthenticateScopedToken
// rejects it before its natural expiry.
func RevokeScopedToken(tokenString string) error {
	var claims scopedClaims
	if _, _, err := jwt.NewParser().ParseUnverified(tokenString, &claims); err != nil {
		return nil // unparseable: nothing to revoke
	}
	if claims.ID == "" {
		return nil
	}
	store := tokenstore.Get()
	if store == nil {
		return errors.New("extension: token store is not initialized")
	}
	return store.Revoke(claims.ID)
}

// scopedPrincipal builds the restricted *model.User a scoped token authenticates
// as: one recursive fs allow-permission per granted path (read, plus the write
// action family when the scope granted write). Its policy IS these rows — see
// op.UserPermissions — so op.Authorize confines it to exactly these paths.
func scopedPrincipal(extID string, paths []string, writes []bool) *model.User {
	perms := make([]model.Permission, 0, len(paths))
	for i, p := range paths {
		write := i < len(writes) && writes[i]
		perms = append(perms, model.Permission{
			ResourceType: model.ResourceFS,
			Name:         "scoped:" + extID,
			Path:         utils.FixAndCleanPath(p),
			Actions:      scopeActions(write),
			Effect:       model.EffectAllow,
			Recursive:    true,
			Origin:       model.OriginManual,
		})
	}
	return &model.User{
		Username:          "ext-scoped:" + extID,
		Scoped:            true,
		ScopedPermissions: perms,
	}
}

func scopeActions(write bool) model.Actions {
	if !write {
		return model.Actions{model.ActRead}
	}
	return model.Actions{
		model.ActRead, model.ActWrite, model.ActRename,
		model.ActMove, model.ActCopy, model.ActRemove,
	}
}

// scopeDeclared reports whether reqPath is equal to or under any declared path
// prefix.
func scopeDeclared(declared []string, reqPath string) bool {
	req := utils.FixAndCleanPath(reqPath)
	for _, d := range declared {
		dp := utils.FixAndCleanPath(d)
		if utils.PathEqual(dp, req) || utils.IsSubPath(dp, req) {
			return true
		}
	}
	return false
}
