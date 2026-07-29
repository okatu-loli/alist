// Package qbittorrent is the concrete qBittorrent OfflineDownloadTool backend
// (子项目 B, wave B4): builtin.offline_download.qbittorrent, an independent,
// manageable built-in extension that hands the qBittorrent WebUI a torrent/magnet
// URL and polls it for status. It talks to qBittorrent through a MINIMAL WebUI
// API v2 client (client.go, stdlib net/http + net/http/cookiejar only — no
// third-party qbit library, because the B2 poll loop uses a ticker and the ext
// contract has no Signal channel), so no new go.mod dependency is introduced.
package qbittorrent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal qBittorrent WebUI API v2 client. It logs in with the
// username/password embedded in the configured WebUI URL's userinfo (auto-login
// on first call, re-login once on a 403), carries the SID cookie in a cookiejar,
// and never sends userinfo on the wire (base has it stripped).
type Client struct {
	base *url.URL
	user string
	pass string
	http *http.Client
}

// New builds a Client from a WebUI URL that MAY embed credentials
// (http://admin:adminadmin@host:8080/). It extracts the username/password from
// the userinfo and strips it from the stored base so requests never carry it in
// the URL. The HTTP client carries a cookiejar (for the SID session cookie) and a
// bounded timeout so a hung WebUI can never wedge a caller.
func New(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	base := *u
	base.User = nil
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		base: &base,
		user: user,
		pass: pass,
		http: &http.Client{Jar: jar, Timeout: 10 * time.Second},
	}, nil
}

// login POSTs /api/v2/auth/login with the username/password form and a Referer of
// the base URL (qBittorrent rejects a login whose Referer/Origin does not match).
// On success the WebUI sets the SID cookie, which the cookiejar captures for
// subsequent calls.
func (c *Client) login() error {
	form := url.Values{}
	form.Set("username", c.user)
	form.Set("password", c.pass)
	u := c.base.JoinPath("/api/v2/auth/login")
	req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.base.String())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbittorrent login failed: http status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// A successful login returns the literal "Ok."; "Fails." means bad creds.
	if !strings.Contains(string(body), "Ok") {
		return fmt.Errorf("qbittorrent login rejected: %s", strings.TrimSpace(string(body)))
	}
	return nil
}

// do issues an authenticated request against path, auto-logging in when no SID
// cookie is present yet and re-logging-in once on a 403 (an expired/absent
// session). It builds each request via newReq so both the first attempt and the
// post-relogin retry send an identical body.
func (c *Client) do(method, path string, newReq func() (*http.Request, error)) (*http.Response, error) {
	// Auto-login on the first call (no SID cookie in the jar yet).
	if len(c.http.Jar.Cookies(c.base)) == 0 {
		if err := c.login(); err != nil {
			return nil, err
		}
	}
	req, err := newReq()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", c.base.String())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		// Session expired/invalid: re-login once and retry.
		resp.Body.Close()
		if err := c.login(); err != nil {
			return nil, err
		}
		req, err = newReq()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Referer", c.base.String())
		resp, err = c.http.Do(req)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// postForm issues an application/x-www-form-urlencoded POST to path.
func (c *Client) postForm(path string, form url.Values) (*http.Response, error) {
	u := c.base.JoinPath(path)
	return c.do(http.MethodPost, path, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	})
}

// AddNewLink adds url (a torrent/magnet/http link) to qBittorrent, downloading
// into savePath and tagging the torrent with category so GetInfo/Delete can find
// it later (qBittorrent's add endpoint returns no id, so the category is the tie
// back to the host-side UID). The request is multipart/form-data, the shape
// qBittorrent's WebUI itself uses for torrents/add.
func (c *Client) AddNewLink(link, savePath, category string) error {
	buf := new(bytes.Buffer)
	writer := multipart.NewWriter(buf)
	fields := [][2]string{
		{"urls", link},
		{"savepath", savePath},
		{"category", category},
		{"autoTMM", "false"},
	}
	for _, f := range fields {
		if err := writer.WriteField(f[0], f[1]); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	contentType := writer.FormDataContentType()
	payload := buf.Bytes()
	u := c.base.JoinPath("/api/v2/torrents/add")
	resp, err := c.do(http.MethodPost, "/api/v2/torrents/add", func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Ok") {
		return fmt.Errorf("qbittorrent add failed for %s: http %d %s", link, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// TorrentInfo is the subset of a qBittorrent torrents/info entry the tool maps.
// Progress is a 0..1 fraction; State is qBittorrent's torrent state string.
type TorrentInfo struct {
	Hash      string  `json:"hash"`
	Name      string  `json:"name"`
	State     string  `json:"state"`
	Progress  float64 `json:"progress"`
	Size      int64   `json:"size"`
	Completed int64   `json:"completed"`
	Category  string  `json:"category"`
	SavePath  string  `json:"save_path"`
}

// GetInfo lists the torrents in category (GET /api/v2/torrents/info?category=).
// An empty slice means the torrent is not visible yet (still fetching metadata /
// queued) — the tool treats that as a non-completed, non-error state.
func (c *Client) GetInfo(category string) ([]TorrentInfo, error) {
	q := url.Values{}
	q.Set("category", category)
	u := c.base.JoinPath("/api/v2/torrents/info")
	u.RawQuery = q.Encode()
	resp, err := c.do(http.MethodGet, "/api/v2/torrents/info", func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, u.String(), nil)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qbittorrent info failed: http status %d", resp.StatusCode)
	}
	var infos []TorrentInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return nil, err
	}
	return infos, nil
}

// Delete removes the torrent hash. deleteFiles MUST be false in this port: the
// downloaded files in savePath (the host temp dir) are still needed by the async
// transfer engine, so deleting them would break the transfer. The bool is kept in
// the signature to mirror the WebUI API and to make the false guarantee explicit
// at every call site.
func (c *Client) Delete(hash string, deleteFiles bool) error {
	form := url.Values{}
	form.Set("hashes", hash)
	if deleteFiles {
		form.Set("deleteFiles", "true")
	} else {
		form.Set("deleteFiles", "false")
	}
	resp, err := c.postForm("/api/v2/torrents/delete", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbittorrent delete failed: http status %d", resp.StatusCode)
	}
	return nil
}

// SetShareLimits best-effort sets the seeding time limit (minutes) for hash
// (POST /api/v2/torrents/setShareLimits). ratioLimit/inactiveSeedingTimeLimit are
// sent as -2 ("use the global limit") so only the seeding time is constrained.
// Used at add time when qbittorrent_seedtime > 0; a failure is non-fatal.
func (c *Client) SetShareLimits(hash string, seedingTimeLimitMinutes int) error {
	form := url.Values{}
	form.Set("hashes", hash)
	form.Set("ratioLimit", "-2")
	form.Set("seedingTimeLimit", fmt.Sprintf("%d", seedingTimeLimitMinutes))
	form.Set("inactiveSeedingTimeLimit", "-2")
	resp, err := c.postForm("/api/v2/torrents/setShareLimits", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qbittorrent setShareLimits failed: http status %d", resp.StatusCode)
	}
	return nil
}

// Version returns the WebUI application version (GET /api/v2/app/version). Used as
// the readiness probe: a failure (unreachable/unauthorized) means the WebUI is
// down or misconfigured.
func (c *Client) Version() (string, error) {
	u := c.base.JoinPath("/api/v2/app/version")
	resp, err := c.do(http.MethodGet, "/api/v2/app/version", func() (*http.Request, error) {
		return http.NewRequest(http.MethodGet, u.String(), nil)
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qbittorrent version failed: http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}
