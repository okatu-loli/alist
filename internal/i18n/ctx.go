package i18n

import "github.com/alist-org/alist/v3/internal/ctxkey"

// langCarrier is the minimal surface needed to read the negotiated locale off
// a request context. *gin.Context satisfies it via GetString, so i18n stays
// decoupled from the web framework.
type langCarrier interface {
	GetString(key string) string
}

// Lang returns the negotiated locale stored on the context by the I18n
// middleware, falling back to DefaultLang when unset.
func Lang(c langCarrier) string {
	if lang := c.GetString(ctxkey.Lang); lang != "" {
		return lang
	}
	return DefaultLang
}
