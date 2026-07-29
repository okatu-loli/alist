package middlewares

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// APIVersion marks every request under a versioned mount so the response
// envelope helpers stamp `version` (see common.RespVersion). Mounted on the
// /api/v4 group.
func APIVersion(version string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(common.APIVersionKey, version)
		c.Next()
	}
}

// Deprecated advertises that the mount is a legacy contract kept alive for
// one major release. Headers follow RFC 8594 (Sunset link relation) shape:
// clients and proxies can detect it mechanically; nothing else changes.
func Deprecated(successorPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Deprecation", "true")
		c.Header("Link", "<"+successorPath+">; rel=\"successor-version\"")
		c.Next()
	}
}

// ParamIntoJSONBody rewrites the request's JSON body so `field` carries the
// path parameter's value — the adapter that lets `PUT /users/:id` (body
// without id) reuse a handler that binds id from the body. A missing or empty
// body becomes `{field: value}`; a non-object body is left untouched (the
// handler's own binding will reject it with its usual error).
func ParamIntoJSONBody(param, field string) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Next()
			return
		}
		var body map[string]any
		if len(bytes.TrimSpace(raw)) == 0 {
			body = map[string]any{}
		} else if err := json.Unmarshal(raw, &body); err != nil || body == nil {
			// Not a JSON object — restore and let the handler's binder complain.
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			c.Next()
			return
		}
		// Path params arrive as strings. Numeric ids must round-trip as JSON
		// numbers (handlers bind uint); everything else stays a string.
		value := c.Param(param)
		if isAllDigits(value) {
			body[field] = json.Number(value)
		} else {
			body[field] = value
		}
		rewritten, err := json.Marshal(body)
		if err != nil {
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			c.Next()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(rewritten))
		c.Request.ContentLength = int64(len(rewritten))
		c.Next()
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
