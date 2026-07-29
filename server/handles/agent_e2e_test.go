package handles

import (
	"encoding/json"
	"github.com/AlistGo/alist-agent/proto"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/gin-gonic/gin"
)

// httpGetJSON/httpPostJSON drive a real HTTP GET/POST against the loopback
// server and decode server/common.Resp[T]'s "data" field into T — the
// e2e-test equivalent of internal/agent's own postJSON, for the admin-side
// endpoints this package's client helpers have no wrapper for.
func httpGetJSON[T any](t *testing.T, url string) (T, error) {
	t.Helper()
	var out T
	resp, err := http.Get(url)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	var env struct {
		Data T `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&env)
	return env.Data, err
}

func httpPostJSON[T any](t *testing.T, url string) (T, error) {
	t.Helper()
	var out T
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	var env struct {
		Data T `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&env)
	return env.Data, err
}

// TestAgentClientJoinPairHeartbeatEndToEnd drives internal/agent's own
// client helpers (the same code cmd/agent.go's `join`/`run` subcommands
// call) against the real handlers over a real loopback HTTP server — proof
// that the wire protocol the client builds is exactly what the server
// accepts, not just that the two sides independently parse their own
// fixtures.
// This is the "join -> pair -> [SAS confirm] -> heartbeat works on
// loopback" scenario called for by the design (W12-2a + W12-2b): pairing
// alone must NOT be enough to reach a working heartbeat — only after the
// admin's SAS-matched confirm does the exact same signed heartbeat that was
// 403'd start succeeding.
func TestAgentClientJoinPairHeartbeatEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := adminUser(t)
	r := gin.New()
	r.POST("/api/v4/agent/pair/complete", PairAgentComplete)
	r.POST("/api/v4/agent/heartbeat", AgentHeartbeat)
	// The admin-facing SAS endpoints, exercised for real over this same
	// loopback server. A minimal middleware standing in for the real
	// session/RequireAdmin gate — this test's focus is the pairing/SAS/
	// heartbeat protocol, not re-proving admin authentication, which is
	// covered end to end elsewhere (server/router_v4_test.go).
	admins := r.Group("/api/v4/admin/agents", func(c *gin.Context) { c.Set(ctxkey.User, admin) })
	admins.GET("/:id/pairing-sas", AgentPairingSAS)
	admins.POST("/:id/confirm", ConfirmAgent)
	srv := httptest.NewServer(r)
	defer srv.Close()

	// design R1: wire a server identity so pair/complete delivers its public
	// key end to end, and assert below that the agent pins it through
	// SaveClientConfig/LoadClientConfig.
	serverIdentity, err := agentsrv.LoadOrCreateServerIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevServerIdentity := agentsrv.CurrentServerIdentity()
	agentsrv.SetServerIdentity(serverIdentity)
	t.Cleanup(func() { agentsrv.SetServerIdentity(prevServerIdentity) })

	// 1. An admin starts a pairing session (the same call StartAgentPairing
	// makes under the hood).
	code, _ := agentsrv.Start()

	// 2. The agent generates/loads its identity (join step 1) — a fresh temp
	// dir stands in for --data.
	dataDir := t.TempDir()
	identity, err := agentpkg.LoadOrCreateIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}

	// 3. The agent completes pairing (join step 2) — the actual HTTP call.
	pairResp, err := agentpkg.PairComplete(t.Context(), srv.URL, agentpkg.PairJoinParams{
		Code:     code,
		Identity: identity,
		Name:     "e2e-nas",
		Platform: "linux/amd64",
		Version:  "vtest",
	})
	if err != nil {
		t.Fatalf("PairComplete: %v", err)
	}
	if pairResp.AgentID == "" {
		t.Fatal("PairComplete returned an empty AgentID")
	}
	// design R1: the response carried the wired server's public key.
	if pairResp.ServerPubKey != serverIdentity.PubKeyBase64() {
		t.Fatalf("pair response server_pubkey = %q, want %q", pairResp.ServerPubKey, serverIdentity.PubKeyBase64())
	}
	wantAgentID := identity.AgentID()
	if pairResp.AgentID != wantAgentID {
		t.Fatalf("server-assigned AgentID %q != identity's own fingerprint %q", pairResp.AgentID, wantAgentID)
	}
	row, err := db.GetAgentByAgentID(pairResp.AgentID)
	if err != nil {
		t.Fatalf("agent row must exist after pairing: %v", err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row.ID) })
	if row.Enabled || row.Confirmed {
		t.Fatalf("a freshly paired agent must be disabled and unconfirmed pending SAS confirm, got enabled=%v confirmed=%v", row.Enabled, row.Confirmed)
	}
	if len(row.Whitelist()) != 0 {
		t.Fatalf("newly paired agent must have an empty whitelist, got %v", row.Whitelist())
	}

	// 3b. The MITM-detection property, pinned end to end over real HTTP with
	// NO tampering in the path: the SAS the agent computes ENTIRELY LOCALLY
	// (its own code + its own generated pubkey — no network round trip
	// needed) must equal what this server independently derived from
	// exactly what it received and is now serving back on the admin's
	// pairing-sas endpoint. A passive/non-tampering path must never produce
	// a false mismatch.
	localSAS := proto.DeriveSAS(code, identity.PubKey)
	if len(pairResp.SAS) != len(localSAS) {
		t.Fatalf("server-returned SAS length = %d, want %d", len(pairResp.SAS), len(localSAS))
	}
	for i := range localSAS {
		if pairResp.SAS[i] != localSAS[i] {
			t.Fatalf("server SAS %v != locally-derived SAS %v (would show a false mismatch on an untampered pairing)", pairResp.SAS, localSAS)
		}
	}
	sasResp, err := httpGetJSON[AgentPairingSASResp](t, srv.URL+"/api/v4/admin/agents/"+strconv.FormatUint(uint64(row.ID), 10)+"/pairing-sas")
	if err != nil {
		t.Fatalf("GET pairing-sas: %v", err)
	}
	if sasResp.Confirmed {
		t.Fatal("pairing-sas must report confirmed=false before the admin confirms")
	}
	if len(sasResp.SAS) != len(localSAS) {
		t.Fatalf("admin-page SAS length = %d, want %d", len(sasResp.SAS), len(localSAS))
	}
	for i := range localSAS {
		if sasResp.SAS[i] != localSAS[i] {
			t.Fatalf("admin-page SAS %v != locally-derived SAS %v", sasResp.SAS, localSAS)
		}
	}

	// 4. join step 3: persist the returned server info — including the pinned
	// server public key (design R1).
	if err := agentpkg.SaveClientConfig(dataDir, agentpkg.ClientConfig{Host: srv.URL, AgentID: pairResp.AgentID, ServerPubKey: pairResp.ServerPubKey}); err != nil {
		t.Fatalf("SaveClientConfig: %v", err)
	}

	// 5. `agent run` loads identity + config back from disk (simulating a
	// process restart between join and run) and sends a real heartbeat —
	// which must be refused (403, disabled) since nobody has confirmed the
	// SAS yet.
	reloadedIdentity, err := agentpkg.LoadOrCreateIdentity(dataDir)
	if err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	cfg, err := agentpkg.LoadClientConfig(dataDir)
	if err != nil {
		t.Fatalf("LoadClientConfig: %v", err)
	}
	// design R1: the pinned server pubkey survived the disk round trip.
	if cfg.ServerPubKey != serverIdentity.PubKeyBase64() {
		t.Fatalf("reloaded config server_pubkey = %q, want %q", cfg.ServerPubKey, serverIdentity.PubKeyBase64())
	}
	if _, err := agentpkg.Heartbeat(t.Context(), cfg.Host, reloadedIdentity, cfg.AgentID); err == nil {
		t.Fatal("heartbeat before SAS confirm must be rejected, got no error")
	}

	// 6. The admin, having visually matched the SAS, confirms — the real
	// endpoint, over real HTTP.
	if _, err := httpPostJSON[AgentDTO](t, srv.URL+"/api/v4/admin/agents/"+strconv.FormatUint(uint64(row.ID), 10)+"/confirm"); err != nil {
		t.Fatalf("POST confirm: %v", err)
	}

	// 7. The EXACT SAME kind of heartbeat that was just refused now
	// succeeds.
	hbResp, err := agentpkg.Heartbeat(t.Context(), cfg.Host, reloadedIdentity, cfg.AgentID)
	if err != nil {
		t.Fatalf("Heartbeat after confirm: %v", err)
	}
	if !hbResp.OK || !hbResp.Enabled {
		t.Fatalf("unexpected heartbeat response: %+v", hbResp)
	}

	reloaded, err := db.GetAgentByAgentID(cfg.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LastSeen == nil {
		t.Fatal("LastSeen must be set after the end-to-end heartbeat")
	}
	if !reloaded.Confirmed {
		t.Fatal("agent row must be Confirmed after the confirm step")
	}

	// 8. A second, distinct heartbeat also succeeds (each call mints a fresh
	// nonce — see internal/agent.Heartbeat) — pins that `agent run`'s
	// heartbeat loop can actually tick more than once, not just once.
	if _, err := agentpkg.Heartbeat(t.Context(), cfg.Host, reloadedIdentity, cfg.AgentID); err != nil {
		t.Fatalf("second Heartbeat: %v", err)
	}
}
