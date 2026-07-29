package handles

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// Remote-agent admin handler tests (design W12-1). These pin the same two
// properties every other admin-DTO test in this package pins for its own
// resource (see sso_admin_test.go / app_password_test.go): the write-only /
// sensitive field never leaks back through a read DTO, and CRUD actually
// mutates the store it claims to. Admin-gating itself (the AuthAdmin/
// RequireAdmin middleware) is exercised end to end in server/router_v4_test.go,
// since these handler-level tests call the gin.HandlerFunc directly (no
// router, no middleware chain) — the same convention every other file in this
// package already follows.

var agentFixtureSeq int64

// agentFixtureAgentID mints a unique AgentID for a fixture row. Deliberately
// NOT derived from t.Name(): TestListAgentsDTOHasNoPubkey's own name
// lowercases to a substring containing "pubkey", which would trip that
// test's own leak check against an unrelated field.
func agentFixtureAgentID(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&agentFixtureSeq, 1)
	return fmt.Sprintf("agent-fixture-%d", n)
}

// seedAgent inserts a row directly through the db layer (this wave has no
// admin endpoint that creates one — that lands with real pairing in W12-2)
// with a secret-looking PubKey, so DTO tests can assert it never surfaces.
func seedAgent(t *testing.T, name string, enabled bool, whitelist []string) *model.Agent {
	t.Helper()
	a := &model.Agent{
		AgentID: agentFixtureAgentID(t),
		Name:    name,
		PubKey:  "SECRET-PUBKEY-MATERIAL-" + agentFixtureAgentID(t),
		Enabled: enabled,
	}
	a.SetWhitelist(whitelist)
	if err := db.CreateAgent(a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(a.ID) })
	return a
}

// drivePatch runs a handler with a JSON body and an :id path param.
func drivePatch(u *model.User, id uint, body string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PATCH", "/x", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(id), 10)}}
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

func parsePairingStartResp(t *testing.T, w *httptest.ResponseRecorder) PairingStartResp {
	t.Helper()
	var env struct {
		Data PairingStartResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse pairing start resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func parseAgentDTOList(t *testing.T, w *httptest.ResponseRecorder) []AgentDTO {
	t.Helper()
	var env struct {
		Data []AgentDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse agent list: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func parseAgentDTO(t *testing.T, w *httptest.ResponseRecorder) AgentDTO {
	t.Helper()
	var env struct {
		Data AgentDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse agent resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestStartAgentPairingReturnsCodeAndExpiry pins the pairing-start response
// shape: a high-entropy code, an expiry ~15 minutes out, and a non-empty
// install hint carrying no secrets (in particular, never the code itself
// embedded where a screenshot/log line could leak it out of band).
func TestStartAgentPairingReturnsCodeAndExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)

	w := driveNoArg(user, StartAgentPairing)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("pairing start must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parsePairingStartResp(t, w)
	if len(resp.Code) < 32 {
		t.Fatalf("pairing code must be high-entropy (>=32 chars), got %q (%d)", resp.Code, len(resp.Code))
	}
	wantExpiry := time.Now().Add(15 * time.Minute)
	if d := wantExpiry.Sub(resp.ExpiresAt); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("expires_at not ~15min out: got %v, want ~%v", resp.ExpiresAt, wantExpiry)
	}
	if resp.InstallHint == "" {
		t.Error("install_hint must not be empty")
	}
	if strings.Contains(resp.InstallHint, resp.Code) {
		t.Error("install_hint must not embed the pairing code")
	}

	// Two starts must never mint the same code.
	w2 := driveNoArg(user, StartAgentPairing)
	resp2 := parsePairingStartResp(t, w2)
	if resp.Code == resp2.Code {
		t.Fatal("two pairing starts minted the same code")
	}
}

// TestListAgentsDTOHasNoPubkey pins the load-bearing DTO property: a listed
// agent never carries its authentication key material, either as a JSON
// field or anywhere in the raw response body.
func TestListAgentsDTOHasNoPubkey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	seeded := seedAgent(t, "nas-1", true, []string{"/local"})

	w := driveNoArg(user, ListAgents)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("list must be 200, got %d: %s", got, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, seeded.PubKey) {
		t.Fatalf("list response must never leak PubKey material: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "pubkey") || strings.Contains(strings.ToLower(body), "pub_key") {
		t.Fatalf("list response must never carry a pubkey field at all: %s", body)
	}

	list := parseAgentDTOList(t, w)
	var found *AgentDTO
	for i := range list {
		if list[i].ID == seeded.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("seeded agent missing from list: %s", body)
	}
	if found.AgentID != seeded.AgentID || found.Name != "nas-1" || !found.Enabled {
		t.Fatalf("DTO fields mismatch: %+v", found)
	}
	if len(found.StorageWhitelist) != 1 || found.StorageWhitelist[0] != "/local" {
		t.Fatalf("DTO whitelist mismatch: %v", found.StorageWhitelist)
	}
}

// TestUpdateAgentPartial pins the partial-update contract: PATCHing only one
// field must leave the others exactly as they were, and each field
// (name/enabled/storage_whitelist) is independently settable.
func TestUpdateAgentPartial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "original-name", true, []string{"/a"})

	// Patch only "enabled": name and whitelist survive untouched.
	w := drivePatch(user, a.ID, `{"enabled":false}`, UpdateAgent)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("patch enabled must be 200, got %d: %s", got, w.Body.String())
	}
	dto := parseAgentDTO(t, w)
	if dto.Enabled {
		t.Fatal("enabled must be false after patch")
	}
	if dto.Name != "original-name" {
		t.Fatalf("name must survive an enabled-only patch, got %q", dto.Name)
	}
	if len(dto.StorageWhitelist) != 1 || dto.StorageWhitelist[0] != "/a" {
		t.Fatalf("whitelist must survive an enabled-only patch, got %v", dto.StorageWhitelist)
	}

	// Patch only "name": enabled stays false from the previous patch.
	w = drivePatch(user, a.ID, `{"name":"renamed"}`, UpdateAgent)
	dto = parseAgentDTO(t, w)
	if dto.Name != "renamed" {
		t.Fatalf("name must apply, got %q", dto.Name)
	}
	if dto.Enabled {
		t.Fatal("enabled must still be false (untouched by a name-only patch)")
	}

	// Patch only "storage_whitelist": everything else stays.
	w = drivePatch(user, a.ID, `{"storage_whitelist":["/b","/c"]}`, UpdateAgent)
	dto = parseAgentDTO(t, w)
	if len(dto.StorageWhitelist) != 2 || dto.StorageWhitelist[0] != "/b" || dto.StorageWhitelist[1] != "/c" {
		t.Fatalf("whitelist patch mismatch: %v", dto.StorageWhitelist)
	}
	if dto.Name != "renamed" {
		t.Fatalf("name must survive a whitelist-only patch, got %q", dto.Name)
	}

	// An empty whitelist explicitly clears it (distinct from omitting the
	// field, which leaves it alone).
	w = drivePatch(user, a.ID, `{"storage_whitelist":[]}`, UpdateAgent)
	dto = parseAgentDTO(t, w)
	if len(dto.StorageWhitelist) != 0 {
		t.Fatalf("explicit empty whitelist must clear it, got %v", dto.StorageWhitelist)
	}
}

// TestUpdateAgentUnknownID pins the not-found shape for an id that never
// existed.
func TestUpdateAgentUnknownID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	w := drivePatch(user, 99999999, `{"name":"x"}`, UpdateAgent)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown id patch must be 404, got %d: %s", got, w.Body.String())
	}
}

// TestDeleteAgentRemoves pins that delete actually removes the row from a
// subsequent list.
func TestDeleteAgentRemoves(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "to-delete", true, nil)

	w := driveDelete(user, a.ID, DeleteAgent)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("delete must be 200, got %d: %s", got, w.Body.String())
	}

	w = driveNoArg(user, ListAgents)
	for _, dto := range parseAgentDTOList(t, w) {
		if dto.ID == a.ID {
			t.Fatalf("deleted agent still present in list: %s", w.Body.String())
		}
	}
}

// --- SAS confirm flow (design W12-2b) ---

// seedUnconfirmedAgent inserts a row shaped exactly like a fresh
// PairAgentComplete left it: disabled, unconfirmed, a pending SAS on file.
func seedUnconfirmedAgent(t *testing.T, name string, sas []string) *model.Agent {
	t.Helper()
	a := &model.Agent{
		AgentID: agentFixtureAgentID(t),
		Name:    name,
		PubKey:  "SECRET-PUBKEY-MATERIAL-" + agentFixtureAgentID(t),
		Enabled: false,
	}
	a.SetWhitelist(nil)
	a.SetPendingSAS(sas)
	if err := db.CreateAgent(a); err != nil {
		t.Fatalf("seed unconfirmed agent: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(a.ID) })
	return a
}

func parseAgentPairingSASResp(t *testing.T, w *httptest.ResponseRecorder) AgentPairingSASResp {
	t.Helper()
	var env struct {
		Data AgentPairingSASResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse pairing-sas resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestAgentPairingSASReturnsStoredValue pins that GET .../pairing-sas
// serves exactly the SAS PairAgentComplete would have persisted — the
// admin's independent view a mismatch against the agent's own terminal
// would be caught against.
func TestAgentPairingSASReturnsStoredValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	sas := []string{"🍎", "🐶", "🐳", "🍇", "🦋"}
	a := seedUnconfirmedAgent(t, "pending-1", sas)

	w := driveDelete(user, a.ID, AgentPairingSAS)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("pairing-sas must be 200, got %d: %s", got, w.Body.String())
	}
	resp := parseAgentPairingSASResp(t, w)
	if resp.Confirmed {
		t.Fatal("a freshly seeded pairing must not be confirmed")
	}
	if len(resp.SAS) != len(sas) {
		t.Fatalf("sas length = %d, want %d", len(resp.SAS), len(sas))
	}
	for i := range sas {
		if resp.SAS[i] != sas[i] {
			t.Fatalf("sas = %v, want %v", resp.SAS, sas)
		}
	}
}

// TestAgentPairingSASUnknownID pins the 404 shape.
func TestAgentPairingSASUnknownID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	w := driveDelete(user, 99999999, AgentPairingSAS)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown id must be 404, got %d: %s", got, w.Body.String())
	}
}

// TestConfirmAgentEnablesAndClearsPendingSAS pins the core SAS-confirm
// contract: confirming an unconfirmed agent enables it and clears its
// pending SAS (nothing left to compare once confirmed).
func TestConfirmAgentEnablesAndClearsPendingSAS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	sas := []string{"🍎", "🐶", "🐳", "🍇", "🦋"}
	a := seedUnconfirmedAgent(t, "pending-2", sas)

	w := driveDelete(user, a.ID, ConfirmAgent)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("confirm must be 200, got %d: %s", got, w.Body.String())
	}
	dto := parseAgentDTO(t, w)
	if !dto.Enabled {
		t.Fatal("confirm must enable the agent")
	}
	if !dto.Confirmed {
		t.Fatal("confirm must mark the agent confirmed")
	}

	row, err := db.GetAgent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.Enabled || !row.Confirmed {
		t.Fatalf("confirm must persist enabled+confirmed, got enabled=%v confirmed=%v", row.Enabled, row.Confirmed)
	}
	if row.PendingSAS != "" {
		t.Fatalf("confirm must clear PendingSAS, got %q", row.PendingSAS)
	}

	// The pairing-sas endpoint now reports confirmed with an empty SAS.
	w2 := driveDelete(user, a.ID, AgentPairingSAS)
	resp := parseAgentPairingSASResp(t, w2)
	if !resp.Confirmed {
		t.Fatal("pairing-sas must report confirmed=true after confirm")
	}
	if len(resp.SAS) != 0 {
		t.Fatalf("pairing-sas must report an empty SAS after confirm, got %v", resp.SAS)
	}
}

// TestConfirmAgentIdempotent pins that confirming an already-confirmed
// agent a second time is a harmless no-op that still reports success.
func TestConfirmAgentIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedUnconfirmedAgent(t, "pending-3", []string{"🍎", "🐶", "🐳", "🍇", "🦋"})

	w1 := driveDelete(user, a.ID, ConfirmAgent)
	if got := respCode(t, w1); got != 200 {
		t.Fatalf("first confirm must be 200, got %d: %s", got, w1.Body.String())
	}

	w2 := driveDelete(user, a.ID, ConfirmAgent)
	if got := respCode(t, w2); got != 200 {
		t.Fatalf("second confirm must also be 200 (idempotent), got %d: %s", got, w2.Body.String())
	}
	dto := parseAgentDTO(t, w2)
	if !dto.Enabled || !dto.Confirmed {
		t.Fatalf("second confirm must still report enabled+confirmed, got %+v", dto)
	}
}

// TestConfirmAgentUnknownID pins the 404 shape.
func TestConfirmAgentUnknownID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	w := driveDelete(user, 99999999, ConfirmAgent)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("unknown id must be 404, got %d: %s", got, w.Body.String())
	}
}
