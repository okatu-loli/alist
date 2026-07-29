package main

import "github.com/alist-org/alist/v3/cmd"

// @title AList V4 API
// @version 4.0
// @description AList V4 HTTP API. All endpoints return the common envelope
// @description `{"code": int, "message": string, "data": T}`; a `code` of 200 means success.
// @description Authenticated endpoints require the `Authorization` header set to the access
// @description token returned by `/api/auth/login` (or `/api/auth/login/hash`).
// @description Base URL is `conf.URL.Path` (empty by default) followed by `/api`, e.g. `/api/me`.
// @termsOfService https://alistgo.com/

// @license.name AGPL-3.0
// @license.url https://github.com/alist-org/alist/blob/main/LICENSE

// @BasePath /api

// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name Authorization
// @description Access token issued by /api/auth/login, /api/auth/login/hash, or /api/auth/refresh.

func main() {
	cmd.Execute()
}
