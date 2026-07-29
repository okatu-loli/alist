package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/gin-gonic/gin"
)

// Cap-token renewal op tests (design cap-renewal). The property under test is
// the SAME "capability is not a bypass" invariant every other cap op enforces
// (see server/handles/agent_task.go's top-of-file doc): renewal must re-run
// resolveTaskCapCore AND BOTH authorization gates (ActRead src, ActWrite dst —
// identical to try-rapid's two gates) under the task creator's LIVE identity,
// plus the terminal-task guard, before it ever reissues a token. A renewed
// token grants exactly the same authority the original one did — never more.

// --- HTTP round trip ---

func TestAgentTaskRenewCapHTTPRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskRenewCap)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}

	var env struct {
		Data AgentTaskRenewCapResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse renew resp: %v (%s)", err, w.Body.String())
	}
	if env.Data.CapToken == "" || env.Data.CapToken == capTok {
		t.Fatalf("expected a FRESH, non-empty token, got %q (old %q)", env.Data.CapToken, capTok)
	}
	if env.Data.ExpiresAt <= 0 {
		t.Fatalf("expected a positive expires_at, got %d", env.Data.ExpiresAt)
	}

	// The renewed token must itself be usable for a subsequent cap op.
	if _, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, env.Data.CapToken); ce != nil {
		t.Fatalf("renewed token must be usable for a later cap op, got %+v", ce.Err)
	}
}

// --- channel round trip ---

func TestChannelRenewCapRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	// The follow-up SrcLink call below falls through to the proxied /d
	// fallback, which needs an absolute site_url (see agent_task_core.go's
	// doc on the site_url root-cause fix) — this test never dials it, only
	// the scheme matters.
	oldSiteURL := conf.Conf.SiteURL
	conf.Conf.SiteURL = "http://alist-test.invalid"
	t.Cleanup(func() { conf.Conf.SiteURL = oldSiteURL })

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	newTok, expiresAt, err := cc.RenewCap(testCtx(t), task.TaskID, capTok)
	if err != nil {
		t.Fatalf("channel RenewCap: %v", err)
	}
	if newTok == "" || newTok == capTok {
		t.Fatalf("expected a fresh token, got %q (old %q)", newTok, capTok)
	}
	if expiresAt <= 0 {
		t.Fatalf("expected a positive expiresAt, got %d", expiresAt)
	}

	// The renewed token must itself work over the channel for a later op.
	if _, err := cc.SrcLink(testCtx(t), task.TaskID, newTok); err != nil {
		t.Fatalf("renewed token must work for a subsequent channel op: %v", err)
	}
}

// TestChannelRenewCapCrossAgentTokenDenied pins the channel-identity defense
// in depth for renewal too: a valid token minted for a DIFFERENT agent is
// refused over this agent's channel.
func TestChannelRenewCapCrossAgentTokenDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fxA := setupAgentTaskFixture(t)
	fxB := setupAgentTaskFixture(t)
	taskB := fxB.createTask(t, model.AgentTaskClaimed)
	capB := fxB.issueCap(t, taskB)

	_, agent := liveReverseChannel(t, fxA.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	if _, _, err := cc.RenewCap(testCtx(t), taskB.TaskID, capB); err == nil {
		t.Fatal("a cap token for a different agent must be refused over this channel")
	}
}

// --- re-auth: choke point + BOTH gates + terminal guard enforced ---

// TestAgentTaskRenewCapTerminalTaskDenied: a settled task rejects renewal
// (409) — the same terminal guard every other cap op enforces.
func TestAgentTaskRenewCapTerminalTaskDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskDone)
	capTok := fx.issueCap(t, task)

	_, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 409 {
		t.Fatalf("terminal task must reject renewal with 409, got %+v", ce)
	}
}

// TestAgentTaskRenewCapDisabledAgentDenied: disabling the agent denies
// renewal — the same live revocation resolveTaskCapCore enforces for every
// other cap op.
func TestAgentTaskRenewCapDisabledAgentDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.agent.Enabled = false
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	_, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil {
		t.Fatal("disabled agent must deny renewal")
	}
}

// TestAgentTaskRenewCapWhitelistRemovedDenied: clearing the whitelist denies
// renewal, same as src-link/put-content/rapid.
func TestAgentTaskRenewCapWhitelistRemovedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.agent.SetWhitelist(nil)
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	_, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 403 {
		t.Fatalf("cleared whitelist must deny renewal with 403, got %+v", ce)
	}
}

// TestAgentTaskRenewCapCreatorDeauthorizedDenied: stripping the creator's
// role denies renewal — op.Authorize runs under the creator's LIVE identity,
// never the token's stale claims.
func TestAgentTaskRenewCapCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	_, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 403 {
		t.Fatalf("de-authorized creator must deny renewal with 403, got %+v", ce)
	}
}

// TestAgentTaskRenewCapDstWriteGateEnforced isolates the DESTINATION-write
// gate: a creator scoped to READ the whole mount but with no write bit passes
// the source-read gate yet must be denied at the dst-write gate — proving
// renewal runs BOTH authorizations, not just the read one (mirrors
// TestAgentTaskTryRapidDstWriteGateEnforced).
func TestAgentTaskRenewCapDstWriteGateEnforced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	roRole := &model.Role{
		Name:             fmt.Sprintf("renew-ro-%d", nextAgentTaskFixtureN(t)),
		PermissionScopes: []model.PermissionEntry{{Path: fx.mount, Permission: 0}},
	}
	if err := op.CreateRole(roRole); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteRole(roRole.ID) })
	fx.creator.Role = model.Roles{int(roRole.ID)}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 403 {
		t.Fatalf("a creator who may read src but not write dst must be denied at the dst-write gate (403), got %+v", ce)
	}
}

// TestAgentTaskRenewCapBoundAgentMismatchDenied: a valid token presented with
// a boundAgentID naming a DIFFERENT agent is refused up front (401).
func TestAgentTaskRenewCapBoundAgentMismatchDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, _, ce := agentTaskRenewCapCore(context.Background(), "some-other-agent", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 401 {
		t.Fatalf("a token redeemed over the wrong agent's channel must be refused (401), got %+v", ce)
	}
}

// TestAgentTaskRenewCapNormalSuccess is the plain success pin: everything
// live/valid, both gates pass, a fresh usable token comes back.
func TestAgentTaskRenewCapNormalSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	newTok, expiresAt, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce != nil {
		t.Fatalf("normal renewal must not error: %+v", ce.Err)
	}
	if newTok == "" || newTok == capTok {
		t.Fatalf("expected a fresh token, got %q (old %q)", newTok, capTok)
	}
	if expiresAt <= 0 {
		t.Fatalf("expected a positive expiresAt, got %d", expiresAt)
	}

	claims, err := agentsrv.VerifyTaskCap(newTok)
	if err != nil {
		t.Fatalf("renewed token must itself verify: %v", err)
	}
	if claims.TaskID != task.TaskID || claims.AgentID != fx.agent.AgentID || claims.Src != task.SrcPath || claims.DstDir != task.DstDir {
		t.Fatalf("renewed token claims mismatch: %+v", claims)
	}
}

// --- total task lifetime ceiling (review fix, confirmed: 续期链无总寿命上限) ---

// backdateTaskCreatedAt rewrites task's CreatedAt directly in the DB (never
// through resolveTaskCapCore's own reload path) so the maxAgentTaskLifetime
// check sees an ACTUALLY old row, exactly as a genuinely long-lived task
// would look — no fake clock needed, since the gate reads the persisted
// column, not any in-memory state.
func backdateTaskCreatedAt(t *testing.T, task *model.AgentTask, age time.Duration) {
	t.Helper()
	task.CreatedAt = time.Now().Add(-age)
	if err := db.UpdateAgentTask(task); err != nil {
		t.Fatal(err)
	}
}

// TestAgentTaskRenewCapExceedsMaxLifetimeDenied pins the fix: a task whose
// CreatedAt is older than maxAgentTaskLifetime is refused renewal with a
// readable, actionable error — even though it is still ACTIVE (not
// terminal) and every other check (whitelist, both authz gates, agent
// enabled) would otherwise pass. This is the only thing bounding a task that
// never reaches a terminal state (stuck mid-copy, or an agent deliberately
// holding one open) from renewing forever — see maxAgentTaskLifetime's doc.
func TestAgentTaskRenewCapExceedsMaxLifetimeDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)
	backdateTaskCreatedAt(t, task, maxAgentTaskLifetime+time.Hour)

	_, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil {
		t.Fatal("a task past maxAgentTaskLifetime must deny renewal")
	}
	if ce.HTTPCode != 401 && ce.HTTPCode != 403 {
		t.Fatalf("expected 401 or 403, got %d (%+v)", ce.HTTPCode, ce)
	}
	if !strings.Contains(ce.Err.Error(), "lifetime") {
		t.Fatalf("error must explain the max-lifetime denial, got %q", ce.Err.Error())
	}
}

// TestAgentTaskRenewCapWithinMaxLifetimeUnaffected: a task safely within the
// ceiling (including one backdated close to, but not past, it) renews
// normally — the gate must not misfire early.
func TestAgentTaskRenewCapWithinMaxLifetimeUnaffected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)
	backdateTaskCreatedAt(t, task, maxAgentTaskLifetime-time.Hour)

	newTok, _, ce := agentTaskRenewCapCore(context.Background(), "", task.TaskID, capTok)
	if ce != nil {
		t.Fatalf("a task within maxAgentTaskLifetime must renew normally, got %+v", ce.Err)
	}
	if newTok == "" || newTok == capTok {
		t.Fatalf("expected a fresh token, got %q (old %q)", newTok, capTok)
	}
}

// TestAgentTaskStatusUnaffectedByMaxLifetime pins the fix's scope: the
// max-lifetime gate applies ONLY to renewal, never to any other cap op — a
// task past the ceiling keeps working on whatever token it is already
// holding (it just can't extend that token's life any further).
func TestAgentTaskStatusUnaffectedByMaxLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)
	backdateTaskCreatedAt(t, task, maxAgentTaskLifetime+time.Hour)

	if _, ce := agentTaskProgressCore(context.Background(), "", task.TaskID, capTok, 50, "still going"); ce != nil {
		t.Fatalf("progress must be unaffected by the task's age, got %+v", ce.Err)
	}
	if _, ce := agentTaskStatusCore(context.Background(), "", task.TaskID, capTok, model.AgentTaskRunning, ""); ce != nil {
		t.Fatalf("status must be unaffected by the task's age, got %+v", ce.Err)
	}
}
