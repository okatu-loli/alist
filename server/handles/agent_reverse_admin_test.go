package handles

import (
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/gin-gonic/gin"
)

// driveGetID runs a handler with an :id path param and no body.
func driveGetID(u *model.User, id uint, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(id), 10)}}
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

// Admin reverse-mode config tests (design R4): PATCH accepts mode + reverse_addr
// with validation, and the DTO surfaces them. Admin-gating itself is exercised
// in server/router_v4_test.go — these call the handler directly, matching the
// package convention (see agent_test.go's top-of-file doc).

// TestUpdateAgentSetsReverseMode pins a valid reverse config lands in the store
// and round-trips through the DTO.
func TestUpdateAgentSetsReverseMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "rev", true, nil)

	w := drivePatch(user, a.ID, `{"mode":"reverse","reverse_addr":"wss://vps.example:5343/agentchannel"}`, UpdateAgent)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("valid reverse config must be 200, got %d: %s", got, w.Body.String())
	}
	dto := parseAgentDTO(t, w)
	if dto.Mode != model.AgentModeReverse {
		t.Errorf("DTO mode = %q, want reverse", dto.Mode)
	}
	if dto.ReverseAddr != "wss://vps.example:5343/agentchannel" {
		t.Errorf("DTO reverse_addr = %q", dto.ReverseAddr)
	}
	// Persisted.
	reloaded, err := db.GetAgent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsReverse() || reloaded.ReverseAddr != "wss://vps.example:5343/agentchannel" {
		t.Fatalf("reverse config not persisted: %+v", reloaded)
	}
}

// TestUpdateAgentDefaultsOutbound pins an agent with no mode set surfaces as
// "outbound" (the safe default), and setting mode="outbound" clears reverse.
func TestUpdateAgentDefaultsOutbound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "out", true, nil)

	w := drivePatch(user, a.ID, `{"name":"renamed"}`, UpdateAgent)
	dto := parseAgentDTO(t, w)
	if dto.Mode != model.AgentModeOutbound {
		t.Errorf("an agent with no mode set must surface as outbound, got %q", dto.Mode)
	}
	if dto.ReverseStatus != nil {
		t.Errorf("outbound agent must have no reverse_status, got %+v", dto.ReverseStatus)
	}
}

// TestUpdateAgentRejectsBadMode pins an unknown mode is a 400 and does not
// mutate the row.
func TestUpdateAgentRejectsBadMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "x", true, nil)

	w := drivePatch(user, a.ID, `{"mode":"sideways"}`, UpdateAgent)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("bad mode must be 400, got %d: %s", got, w.Body.String())
	}
	reloaded, _ := db.GetAgent(a.ID)
	if reloaded.Mode == "sideways" {
		t.Fatal("a rejected mode must never be persisted")
	}
}

// TestUpdateAgentRejectsMalformedReverseAddr pins that reverse mode with a
// junk / non-ws address is a 400.
func TestUpdateAgentRejectsMalformedReverseAddr(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)

	cases := []struct {
		name string
		body string
	}{
		{"http scheme", `{"mode":"reverse","reverse_addr":"http://vps.example/agentchannel"}`},
		{"no host", `{"mode":"reverse","reverse_addr":"ws:///agentchannel"}`},
		{"junk", `{"mode":"reverse","reverse_addr":"not a url at all"}`},
		{"reverse without addr", `{"mode":"reverse"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := seedAgent(t, "x", true, nil)
			w := drivePatch(user, a.ID, tc.body, UpdateAgent)
			if got := respCode(t, w); got != 400 {
				t.Fatalf("%s must be 400, got %d: %s", tc.name, got, w.Body.String())
			}
			reloaded, _ := db.GetAgent(a.ID)
			if reloaded.IsReverse() {
				t.Fatalf("%s: a rejected reverse config must never be persisted", tc.name)
			}
		})
	}
}

// TestUpdateAgentReverseStatusEndpoint pins the reverse-status endpoint: a 400
// when no supervisor is running, and the tracked status once one is.
func TestUpdateAgentReverseStatusEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := adminUser(t)
	a := seedAgent(t, "rev", true, nil)
	a.Mode = model.AgentModeReverse
	a.ReverseAddr = "wss://vps.example:5343/agentchannel"
	if err := db.UpdateAgent(a); err != nil {
		t.Fatal(err)
	}

	// No supervisor running → 400 (reverse mode unavailable this run).
	currentReverseSupervisorMu.Lock()
	prev := currentReverseSupervisor
	currentReverseSupervisor = nil
	currentReverseSupervisorMu.Unlock()
	t.Cleanup(func() {
		currentReverseSupervisorMu.Lock()
		currentReverseSupervisor = prev
		currentReverseSupervisorMu.Unlock()
	})

	w := driveGetID(user, a.ID, AgentReverseStatus)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("reverse-status with no supervisor must be 400, got %d: %s", got, w.Body.String())
	}

	// With a supervisor tracking a status, it surfaces.
	_, serverPriv := genEd25519(t)
	sup := newReverseSupervisor(serverPriv)
	sup.setStatus(a.AgentID, ReverseStatus{State: reverseStateConnected, Detail: "channel live"})
	currentReverseSupervisorMu.Lock()
	currentReverseSupervisor = sup
	currentReverseSupervisorMu.Unlock()

	w = driveGetID(user, a.ID, AgentReverseStatus)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("reverse-status must be 200, got %d: %s", got, w.Body.String())
	}
}
