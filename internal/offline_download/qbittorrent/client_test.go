package qbittorrent

import (
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mockQbit is an httptest qBittorrent WebUI v2 double. It requires a valid SID
// cookie on every non-login endpoint (returning 403 otherwise, exactly like the
// real WebUI), records the last add/delete form fields, and lets a test dictate
// the torrents/info payload and the per-call login counter.
type mockQbit struct {
	srv *httptest.Server

	mu         sync.Mutex
	logins     int
	addFields  map[string]string
	deleteForm map[string]string
	infoJSON   string
	// forceForbidUntilRelogin makes the first authed request 403 even with a
	// cookie, to exercise the re-login-on-403 path; cleared after one relogin.
	forbidNextAuthed bool
}

const testSID = "SID=abc123"

func newMockQbit(t *testing.T) *mockQbit {
	t.Helper()
	m := &mockQbit{addFields: map[string]string{}, deleteForm: map[string]string{}, infoJSON: "[]"}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.logins++
		m.mu.Unlock()
		_ = r.ParseForm()
		if r.PostFormValue("username") == "" {
			// still respond Ok so tests with empty creds work
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "abc123", Path: "/"})
		_, _ = w.Write([]byte("Ok."))
	})

	authed := func(w http.ResponseWriter, r *http.Request) bool {
		ck, err := r.Cookie("SID")
		if err != nil || ck.Value == "" {
			w.WriteHeader(http.StatusForbidden)
			return false
		}
		m.mu.Lock()
		forbid := m.forbidNextAuthed
		m.forbidNextAuthed = false
		m.mu.Unlock()
		if forbid {
			w.WriteHeader(http.StatusForbidden)
			return false
		}
		return true
	}

	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		_, _ = w.Write([]byte("v4.6.0"))
	})

	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		ct := r.Header.Get("Content-Type")
		mt, params, _ := mime.ParseMediaType(ct)
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			form, _ := mr.ReadForm(1 << 20)
			m.mu.Lock()
			for k, v := range form.Value {
				if len(v) > 0 {
					m.addFields[k] = v[0]
				}
			}
			m.mu.Unlock()
		}
		_, _ = w.Write([]byte("Ok."))
	})

	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		m.mu.Lock()
		body := m.infoJSON
		m.mu.Unlock()
		_ = r.URL.Query().Get("category")
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		_ = r.ParseForm()
		m.mu.Lock()
		for k := range r.PostForm {
			m.deleteForm[k] = r.PostFormValue(k)
		}
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/v2/torrents/setShareLimits", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// urlWithCreds embeds admin:adminadmin into the mock server URL's userinfo.
func (m *mockQbit) urlWithCreds() string {
	// m.srv.URL is like http://127.0.0.1:PORT
	return strings.Replace(m.srv.URL, "http://", "http://admin:adminadmin@", 1)
}

func TestCredentialExtraction(t *testing.T) {
	c, err := New("http://admin:adminadmin@localhost:8080/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.user != "admin" || c.pass != "adminadmin" {
		t.Fatalf("creds = %q/%q, want admin/adminadmin", c.user, c.pass)
	}
	if c.base.User != nil {
		t.Fatalf("base still carries userinfo: %v", c.base.User)
	}
	if c.base.String() != "http://localhost:8080/" {
		t.Fatalf("base = %q, want http://localhost:8080/", c.base.String())
	}
}

func TestClientLoginAndVersion(t *testing.T) {
	m := newMockQbit(t)
	c, err := New(m.urlWithCreds())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := c.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v != "v4.6.0" {
		t.Fatalf("version = %q, want v4.6.0", v)
	}
	// Auto-login fired exactly once; the SID cookie is now in the jar.
	m.mu.Lock()
	logins := m.logins
	m.mu.Unlock()
	if logins != 1 {
		t.Fatalf("logins = %d, want 1 (auto-login on first call)", logins)
	}
	if len(c.http.Jar.Cookies(c.base)) == 0 {
		t.Fatal("expected SID cookie in the jar after login")
	}
}

func TestClientReloginOn403(t *testing.T) {
	m := newMockQbit(t)
	c, err := New(m.urlWithCreds())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Prime the session.
	if _, err := c.Version(); err != nil {
		t.Fatalf("Version: %v", err)
	}
	// Next authed request 403s once → client must re-login and retry.
	m.mu.Lock()
	m.forbidNextAuthed = true
	before := m.logins
	m.mu.Unlock()

	if _, err := c.Version(); err != nil {
		t.Fatalf("Version after forced 403: %v", err)
	}
	m.mu.Lock()
	after := m.logins
	m.mu.Unlock()
	if after != before+1 {
		t.Fatalf("logins = %d, want %d (one re-login on 403)", after, before+1)
	}
}

func TestClientAddNewLink(t *testing.T) {
	m := newMockQbit(t)
	c, err := New(m.urlWithCreds())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.AddNewLink("magnet:?xt=urn:btih:deadbeef", "/tmp/dl/uid1", "uid1"); err != nil {
		t.Fatalf("AddNewLink: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addFields["urls"] != "magnet:?xt=urn:btih:deadbeef" {
		t.Fatalf("urls = %q", m.addFields["urls"])
	}
	if m.addFields["savepath"] != "/tmp/dl/uid1" {
		t.Fatalf("savepath = %q, want /tmp/dl/uid1", m.addFields["savepath"])
	}
	if m.addFields["category"] != "uid1" {
		t.Fatalf("category = %q, want uid1", m.addFields["category"])
	}
}

func TestClientGetInfo(t *testing.T) {
	m := newMockQbit(t)
	m.infoJSON = `[{"hash":"HASH1","name":"file.iso","state":"downloading","progress":0.5,"size":2000,"completed":1000,"category":"uid1"}]`
	c, err := New(m.urlWithCreds())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	infos, err := c.GetInfo("uid1")
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("len = %d, want 1", len(infos))
	}
	got := infos[0]
	if got.Hash != "HASH1" || got.State != "downloading" || got.Progress != 0.5 || got.Size != 2000 || got.Completed != 1000 {
		t.Fatalf("info = %+v", got)
	}
}

func TestClientGetInfoEmpty(t *testing.T) {
	m := newMockQbit(t)
	m.infoJSON = `[]`
	c, err := New(m.urlWithCreds())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	infos, err := c.GetInfo("nope")
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("len = %d, want 0", len(infos))
	}
}

// TestClientDeleteSendsDeleteFilesFalse proves the wire form ALWAYS carries
// deleteFiles=false: the downloaded temp files must survive for the transfer.
func TestClientDeleteSendsDeleteFilesFalse(t *testing.T) {
	m := newMockQbit(t)
	c, err := New(m.urlWithCreds())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Delete("HASH1", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteForm["hashes"] != "HASH1" {
		t.Fatalf("hashes = %q, want HASH1", m.deleteForm["hashes"])
	}
	if m.deleteForm["deleteFiles"] != "false" {
		t.Fatalf("deleteFiles = %q, want false", m.deleteForm["deleteFiles"])
	}
}
