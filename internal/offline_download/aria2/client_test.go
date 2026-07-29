package aria2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// rpcRequest is the wire shape the client sends; a mock server decodes it to
// assert method + params (incl. the token:secret auth prefix).
type rpcRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

// mockAria2 is an httptest JSON-RPC/HTTP aria2 double. It records the last
// request and lets a test dictate the result payload per method.
type mockAria2 struct {
	srv     *httptest.Server
	lastReq rpcRequest
	// results maps method -> raw JSON result to echo back.
	results map[string]string
}

func newMockAria2(t *testing.T) *mockAria2 {
	t.Helper()
	m := &mockAria2{results: map[string]string{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.lastReq = req
		if req.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q, want 2.0", req.JSONRPC)
		}
		result, ok := m.results[req.Method]
		if !ok {
			// Unknown method → JSON-RPC error object.
			_, _ = w.Write([]byte(`{"error":{"code":1,"message":"unknown method ` + req.Method + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":` + result + `}`))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// params returns the decoded params of the last request as Go values (numbers
// as float64, etc.) via generic json.Unmarshal per element.
func (m *mockAria2) paramStrings() []string {
	out := make([]string, 0, len(m.lastReq.Params))
	for _, p := range m.lastReq.Params {
		var s string
		if err := json.Unmarshal(p, &s); err == nil {
			out = append(out, s)
			continue
		}
		out = append(out, string(p))
	}
	return out
}

func TestClientAddURI(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.addUri"] = `"abcd1234"`

	c := New(m.srv.URL, "")
	gid, err := c.AddURI([]string{"http://example.com/f.bin"}, map[string]string{"dir": "/tmp/x"})
	if err != nil {
		t.Fatalf("AddURI: %v", err)
	}
	if gid != "abcd1234" {
		t.Fatalf("gid = %q, want abcd1234", gid)
	}
	if m.lastReq.Method != "aria2.addUri" {
		t.Fatalf("method = %q, want aria2.addUri", m.lastReq.Method)
	}
	// No secret → first param is the uris array.
	if len(m.lastReq.Params) != 2 {
		t.Fatalf("params len = %d, want 2 (uris, options)", len(m.lastReq.Params))
	}
	var uris []string
	if err := json.Unmarshal(m.lastReq.Params[0], &uris); err != nil {
		t.Fatalf("param[0] uris: %v", err)
	}
	if !reflect.DeepEqual(uris, []string{"http://example.com/f.bin"}) {
		t.Fatalf("uris = %v", uris)
	}
	var opts map[string]string
	if err := json.Unmarshal(m.lastReq.Params[1], &opts); err != nil {
		t.Fatalf("param[1] options: %v", err)
	}
	if opts["dir"] != "/tmp/x" {
		t.Fatalf("options.dir = %q, want /tmp/x", opts["dir"])
	}
}

func TestClientTokenAuthPrefix(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.addUri"] = `"g"`

	c := New(m.srv.URL, "s3cr3t")
	if _, err := c.AddURI([]string{"http://x"}, nil); err != nil {
		t.Fatalf("AddURI: %v", err)
	}
	// With a secret, "token:s3cr3t" is prepended as the FIRST param.
	if len(m.lastReq.Params) != 3 {
		t.Fatalf("params len = %d, want 3 (token, uris, options)", len(m.lastReq.Params))
	}
	var tok string
	if err := json.Unmarshal(m.lastReq.Params[0], &tok); err != nil {
		t.Fatalf("param[0] token: %v", err)
	}
	if tok != "token:s3cr3t" {
		t.Fatalf("token param = %q, want token:s3cr3t", tok)
	}
}

func TestClientTellStatus(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.tellStatus"] = `{"gid":"g1","status":"active","totalLength":"1000","completedLength":"400","errorCode":"0","errorMessage":"","followedBy":["g2"]}`

	c := New(m.srv.URL, "")
	st, err := c.TellStatus("g1")
	if err != nil {
		t.Fatalf("TellStatus: %v", err)
	}
	if st.Gid != "g1" || st.Status != "active" || st.TotalLength != "1000" || st.CompletedLength != "400" {
		t.Fatalf("status = %+v", st)
	}
	if !reflect.DeepEqual(st.FollowedBy, []string{"g2"}) {
		t.Fatalf("followedBy = %v", st.FollowedBy)
	}
	// TellStatus params: [gid, keys...] (keys as a nested array).
	if m.lastReq.Method != "aria2.tellStatus" {
		t.Fatalf("method = %q", m.lastReq.Method)
	}
	var gid string
	if err := json.Unmarshal(m.lastReq.Params[0], &gid); err != nil || gid != "g1" {
		t.Fatalf("param[0] gid = %q err=%v", gid, err)
	}
	var keys []string
	if err := json.Unmarshal(m.lastReq.Params[1], &keys); err != nil {
		t.Fatalf("param[1] keys: %v", err)
	}
	for _, want := range []string{"gid", "status", "totalLength", "completedLength", "errorCode", "errorMessage", "followedBy"} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tellStatus keys missing %q, got %v", want, keys)
		}
	}
}

func TestClientRemoveAndRemoveDownloadResult(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.remove"] = `"g1"`
	m.results["aria2.removeDownloadResult"] = `"OK"`

	c := New(m.srv.URL, "")
	if err := c.Remove("g1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if m.lastReq.Method != "aria2.remove" {
		t.Fatalf("method = %q, want aria2.remove", m.lastReq.Method)
	}
	if got := m.paramStrings(); len(got) != 1 || got[0] != "g1" {
		t.Fatalf("remove params = %v, want [g1]", got)
	}
	if err := c.RemoveDownloadResult("g1"); err != nil {
		t.Fatalf("RemoveDownloadResult: %v", err)
	}
	if m.lastReq.Method != "aria2.removeDownloadResult" {
		t.Fatalf("method = %q, want aria2.removeDownloadResult", m.lastReq.Method)
	}
}

func TestClientGetVersion(t *testing.T) {
	m := newMockAria2(t)
	m.results["aria2.getVersion"] = `{"version":"1.36.0","enabledFeatures":["BitTorrent"]}`

	c := New(m.srv.URL, "")
	v, err := c.GetVersion()
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if v != "1.36.0" {
		t.Fatalf("version = %q, want 1.36.0", v)
	}
}

func TestClientRPCError(t *testing.T) {
	m := newMockAria2(t)
	// No result registered for tellStatus → server returns an error object.
	c := New(m.srv.URL, "")
	if _, err := c.TellStatus("nope"); err == nil {
		t.Fatal("expected a JSON-RPC error to surface as a Go error")
	}
}
