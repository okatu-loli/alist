// Package aria2 is the concrete aria2 OfflineDownloadTool backend (子项目 B, wave
// B3): builtin.offline_download.aria2, an independent, manageable built-in
// extension that hands the aria2 daemon a URL over JSON-RPC and polls it for
// status. It talks to aria2 through a MINIMAL JSON-RPC-over-HTTP client
// (client.go, stdlib net/http only — no websocket/notification code, because the
// B2 poll loop uses a ticker and the ext contract has no Signal channel), so no
// new go.mod dependency is introduced.
package aria2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is a minimal aria2 JSON-RPC-over-HTTP client. Every method POSTs a
// single {"jsonrpc":"2.0",...} request to uri and parses {"result"} / {"error"}.
// When secret != "" the aria2 auth convention prepends "token:"+secret as the
// first param of every call.
type Client struct {
	uri    string
	secret string
	http   *http.Client
}

// New builds a Client for the given aria2 JSON-RPC endpoint and rpc secret
// ("" = no secret). The HTTP client carries a bounded timeout so a hung daemon
// can never wedge a caller.
func New(uri, secret string) *Client {
	return &Client{
		uri:    uri,
		secret: secret,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

// rpcID is a constant JSON-RPC id: this client issues one request per HTTP round
// trip and never correlates responses out of band, so a fixed id is sufficient.
const rpcID = "alist"

type rpcRequestBody struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      string        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("aria2 rpc error %d: %s", e.Code, e.Message)
}

type rpcResponseBody struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// call performs one JSON-RPC round trip: it prepends the token param when a
// secret is set, POSTs the request, and unmarshals result into out (out may be
// nil to discard). A transport failure, a non-2xx status, or an {"error"} body
// all surface as a Go error.
func (c *Client) call(method string, params []interface{}, out interface{}) error {
	allParams := params
	if c.secret != "" {
		allParams = append([]interface{}{"token:" + c.secret}, params...)
	}
	if allParams == nil {
		// aria2 expects an array; a nil slice would marshal to null.
		allParams = []interface{}{}
	}
	body, err := json.Marshal(rpcRequestBody{
		JSONRPC: "2.0",
		ID:      rpcID,
		Method:  method,
		Params:  allParams,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.uri, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("aria2 rpc: unexpected http status %d", resp.StatusCode)
	}
	var parsed rpcResponseBody
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return err
	}
	if parsed.Error != nil {
		return parsed.Error
	}
	if out != nil && len(parsed.Result) > 0 {
		return json.Unmarshal(parsed.Result, out)
	}
	return nil
}

// AddURI queues a download of uris with options (e.g. {"dir": tempDir}) and
// returns the daemon-side gid (aria2.addUri, params [uris, options]).
func (c *Client) AddURI(uris []string, options map[string]string) (string, error) {
	if options == nil {
		options = map[string]string{}
	}
	var gid string
	if err := c.call("aria2.addUri", []interface{}{uris, options}, &gid); err != nil {
		return "", err
	}
	return gid, nil
}

// StatusResult is the subset of aria2.tellStatus fields the tool needs. aria2
// returns the numeric fields as JSON strings; they are kept as strings here and
// parsed to int64 by the caller (the tool).
type StatusResult struct {
	Gid             string   `json:"gid"`
	Status          string   `json:"status"`
	TotalLength     string   `json:"totalLength"`
	CompletedLength string   `json:"completedLength"`
	ErrorCode       string   `json:"errorCode"`
	ErrorMessage    string   `json:"errorMessage"`
	FollowedBy      []string `json:"followedBy"`
}

// statusKeys is the explicit key set requested from aria2.tellStatus so the
// daemon returns only the fields the tool maps (smaller payloads, stable shape).
var statusKeys = []string{"gid", "status", "totalLength", "completedLength", "errorCode", "errorMessage", "followedBy"}

// TellStatus reports the current state of gid (aria2.tellStatus, params
// [gid, keys]).
func (c *Client) TellStatus(gid string) (*StatusResult, error) {
	var res StatusResult
	if err := c.call("aria2.tellStatus", []interface{}{gid, statusKeys}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Remove stops the download gid (aria2.remove). aria2's remove STOPS the
// download but does NOT delete already-downloaded files — that is required: the
// async transfer still needs the temp files.
func (c *Client) Remove(gid string) error {
	var res string
	return c.call("aria2.remove", []interface{}{gid}, &res)
}

// RemoveDownloadResult clears the completed/error record for gid
// (aria2.removeDownloadResult). Like Remove, it does NOT delete downloaded files.
func (c *Client) RemoveDownloadResult(gid string) error {
	var res string
	return c.call("aria2.removeDownloadResult", []interface{}{gid}, &res)
}

// GetVersion returns the daemon version string (aria2.getVersion). Used as the
// readiness probe: a failure means the daemon is unreachable/misconfigured.
func (c *Client) GetVersion() (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.call("aria2.getVersion", nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}
