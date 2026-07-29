package hostcap

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/sign"
)

// hostSign is the borrowed Signer. Invariant 4: SignLink authorizes READ on the
// referenced object with the plugin's principal BEFORE signing, so a plugin
// cannot mint a link to an object outside its grant. The signing key stays in
// the core (internal/sign) — the plugin never sees it; it only receives an
// opaque token.
type hostSign struct {
	grant Grant
	prin  *principals
	bound *model.User
}

func newHostSign(grant Grant, prin *principals) *hostSign {
	return &hostSign{grant: grant, prin: prin, bound: boundPrincipal(grant.PluginID, grant.FS)}
}

func (s *hostSign) principal(p ext.Principal) *model.User {
	if p == "" {
		return s.bound
	}
	return s.prin.lookup(string(p))
}

// tokenSep separates the base64 ref payload from the core signature in a link
// token. The payload is not secret (it only names the ref+scope); the signature
// binds it and carries the expiry, so tampering with the payload invalidates the
// token.
const tokenSep = "."

func (s *hostSign) SignLink(ctx context.Context, p ext.Principal, ref ext.ObjRef, scope ext.SignScope, ttl time.Duration) (string, error) {
	if !s.grant.HasAPI(CapSign) {
		return "", ext.ErrPermissionDenied
	}
	user := s.principal(p)
	if user == nil {
		return "", ext.ErrPermissionDenied
	}
	if err := op.Authorize(ctx, user, model.ActRead, op.AuthzRequest{Path: refPath(ref)}); err != nil {
		return "", ext.ErrPermissionDenied
	}
	payload := encodeRefScope(ref, scope)
	var signature string
	if ttl > 0 {
		signature = sign.WithDuration(payload, ttl)
	} else {
		signature = sign.NotExpired(payload)
	}
	if signature == "" {
		return "", ext.NewError(ext.ErrCodeNotSupport, "no download signer available")
	}
	b64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return b64 + tokenSep + signature, nil
}

func (s *hostSign) VerifyLink(_ context.Context, token string) (ext.ObjRef, ext.SignScope, error) {
	if !s.grant.HasAPI(CapSign) {
		return ext.ObjRef{}, "", ext.ErrPermissionDenied
	}
	b64, signature, ok := strings.Cut(token, tokenSep)
	if !ok {
		return ext.ObjRef{}, "", ext.NewError(ext.ErrCodeNotSupport, "malformed token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return ext.ObjRef{}, "", ext.NewError(ext.ErrCodeNotSupport, "malformed token")
	}
	payload := string(raw)
	if err := sign.Verify(payload, signature); err != nil {
		return ext.ObjRef{}, "", ext.ErrPermissionDenied
	}
	ref, scope := decodeRefScope(payload)
	return ref, scope, nil
}

// encodeRefScope serializes a ref+scope into the signed payload. Fields are
// newline-separated (paths and scopes never contain newlines) with a version
// tag so the format can evolve.
func encodeRefScope(ref ext.ObjRef, scope ext.SignScope) string {
	return strings.Join([]string{"v1", ref.StorageID, ref.Anchor, string(scope)}, "\n")
}

func decodeRefScope(payload string) (ext.ObjRef, ext.SignScope) {
	parts := strings.SplitN(payload, "\n", 4)
	if len(parts) < 4 {
		return ext.ObjRef{}, ""
	}
	return ext.ObjRef{StorageID: parts[1], Anchor: parts[2]}, ext.SignScope(parts[3])
}
