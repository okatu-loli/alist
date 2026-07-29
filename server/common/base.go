package common

import (
	"fmt"
	"net/http"
	stdpath "path"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
)

func GetApiUrl(r *http.Request) string {
	api := conf.Conf.SiteURL
	if strings.HasPrefix(api, "http") {
		return strings.TrimSuffix(api, "/")
	}
	if r != nil {
		protocol := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			protocol = "https"
		}
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		api = fmt.Sprintf("%s://%s", protocol, stdpath.Join(host, api))
	}
	api = strings.TrimSuffix(api, "/")
	return api
}

// CompleteLink makes a driver-relative URL absolute against the request's API
// base before it is handed to a client. Drivers may return URLs like "/p/...";
// only the HTTP layer knows the base, so completion lives here, not in fs.
func CompleteLink(r *http.Request, l *model.Link) {
	if l == nil || l.URL == "" {
		return
	}
	if !strings.HasPrefix(l.URL, "http://") && !strings.HasPrefix(l.URL, "https://") {
		l.URL = GetApiUrl(r) + l.URL
	}
}
