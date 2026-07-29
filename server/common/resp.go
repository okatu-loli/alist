package common

import "github.com/gin-gonic/gin"

type Resp[T any] struct {
	// Version identifies the API contract that produced this envelope. It is
	// stamped only on versioned mounts (`/api/v4` → "v4"); legacy `/api`
	// responses omit it (omitempty), so v3 clients see no shape change.
	Version string `json:"version,omitempty"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type PageResp struct {
	Content interface{} `json:"content"`
	Total   int64       `json:"total"`
}

// APIVersionKey is the gin ctx key carrying the API version of the current
// mount; set by middlewares.APIVersion on the /api/v4 group.
const APIVersionKey = "api_version"

// RespVersion returns the version string the current request's envelope
// should carry ("" outside a versioned mount).
func RespVersion(c *gin.Context) string {
	return c.GetString(APIVersionKey)
}
