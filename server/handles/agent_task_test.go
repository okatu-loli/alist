package handles

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/AlistGo/alist-agent/proto"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/alist-org/alist/v3/drivers" // registers the Local driver (init side effect)
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/sign"
	"github.com/gin-gonic/gin"
)

// Remote-agent task-delegation handler tests (design W12-3a): task
// creation (authed) + the four cap_token-authed agent-facing capability
// endpoints. The core property under test throughout is "capability is not
// a bypass" — see server/handles/agent_task.go's top-of-file doc: every
// agent-facing call must re-verify the token, the live task/agent state,
// the storage whitelist, AND op.Authorize against the task's CREATOR, and a
// failure of ANY one of those must deny the call even when everything else
// about it is perfectly valid.

var agentTaskFixtureSeq int64

func nextAgentTaskFixtureN(t *testing.T) int64 {
	t.Helper()
	return atomic.AddInt64(&agentTaskFixtureSeq, 1)
}

// agentTaskFixture bundles everything one test needs: a real Local storage
// mount with a source file and a destination directory, a REAL PERSISTED
// user (op.CreateUser, not the request-scoped Scoped-principal pattern —
// resolveTaskCap loads the creator fresh via op.GetUserById, which requires
// an actual row) authorized for the whole mount (read+write), and a paired,
// enabled, whitelisted agent.
type agentTaskFixture struct {
	mount   string
	diskDir string // the Local storage's root_folder_path, for asserting on-disk effects directly
	srcPath string
	dstDir  string
	creator *model.User
	roleID  uint
	agent   *model.Agent
}

// createSafeMountRole creates a role granting mount read+write, guarding
// against an auto-incremented role id accidentally landing on a legacy
// role-tier sentinel value (model.GENERAL=0, GUEST=1, ADMIN=2,
// NEWGENERAL=3): model.User.Role is the SAME []int used both for RBAC
// role-table foreign keys (what this binds the fixture's user to) and for
// those legacy sentinels (model.Roles.Contains / IsAdmin / IsGuest) — an id
// that happens to equal 2 would silently make any enabled user bound to it
// satisfy IsAdmin(), corrupting unrelated tests that count enabled admins
// (e.g. lockout_test.go) in this package's ONE shared, whole-test-binary
// sqlite db (permission_test.go's init()). Some OTHER test also explicitly
// seeds a role row AT the literal id 2 (authn_redteam_test.go's
// makeDefaultRoleAdmin, simulating a misconfigured default-role admin
// backstop) and needs that id to still be free when it runs — so the fix
// here is not to reserve/burn the low ids (that would permanently deny them
// to that fixture) but to never let one of THIS file's roles keep
// occupying one: ids increase by exactly 1 per sequential create with no
// interleaving (this loop runs single-threaded within one test), so a
// short create/delete probe is guaranteed to reach an id above the
// sentinel range within a handful of attempts, and every id ≤3 it touched
// along the way is deleted again immediately — never left alive, not even
// temporarily past this function's return.
func createSafeMountRole(t *testing.T, n int64, mount string) *model.Role {
	t.Helper()
	var lowIDs []uint
	for i := 0; i < 8; i++ {
		role := &model.Role{
			Name: fmt.Sprintf("atask-role-%d-%d", n, i),
			PermissionScopes: []model.PermissionEntry{
				// bit 3 = ActWrite (see internal/model/action.go's
				// LegacyBitActions); ActRead is granted implicitly for every
				// scope-covered path (same convention as authz_matrix_test.go's
				// setupMatrix).
				{Path: mount, Permission: 1 << 3},
			},
		}
		if err := op.CreateRole(role); err != nil {
			t.Fatal(err)
		}
		if role.ID > uint(model.NEWGENERAL) {
			for _, id := range lowIDs {
				_ = op.DeleteRole(id)
			}
			t.Cleanup(func() { _ = op.DeleteRole(role.ID) })
			return role
		}
		lowIDs = append(lowIDs, role.ID)
	}
	t.Fatal("createSafeMountRole: could not obtain a role id past the legacy sentinel range")
	return nil
}

// setupAgentTaskFixture mounts an isolated Local storage with a readable
// source file and a destination directory, a role granting the WHOLE mount
// read+write, a persisted user bound to that role, and a paired agent whose
// whitelist covers the mount (by mount-path form).
func setupAgentTaskFixture(t *testing.T) agentTaskFixture {
	t.Helper()
	n := nextAgentTaskFixtureN(t)
	mount := fmt.Sprintf("/atask%d", n)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.bin"), []byte("agent task payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), sid) })

	role := createSafeMountRole(t, n, mount)

	creator := &model.User{Username: fmt.Sprintf("atask-user-%d", n), Role: model.Roles{int(role.ID)}}
	if err := op.CreateUser(creator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(creator.ID) })

	a := &model.Agent{AgentID: fmt.Sprintf("atask-agent-%d", n), Name: "task-agent", Enabled: true}
	a.SetWhitelist([]string{mount})
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(a.ID) })

	return agentTaskFixture{
		mount:   mount,
		diskDir: dir,
		srcPath: mount + "/file.bin",
		dstDir:  mount + "/dst",
		creator: creator,
		roleID:  role.ID,
		agent:   a,
	}
}

// setupBalanceAgentTaskFixture mirrors setupAgentTaskFixture but mounts a
// SIMULATED two-member balance group instead of a single storage: both
// members share the same actual/virtual mount path (mount) but have
// distinct MountPath strings (mount, mount+".balance1") and distinct storage
// ids — exactly the shape op.GetBalancedStorage round-robins across on every
// resolution. The agent's whitelist is a single PATH-form entry for the bare
// mount ("/atbal<n>", no ".balanceN" suffix), which is exactly the form an
// admin would type expecting it to cover "the /atbal<n> mount" as a whole —
// this is FIX 3b's regression fixture: both members' underlying dirs get an
// identical source file and an existing dst dir, so a read/write succeeds
// (and is content-verifiable) no matter which member's round-robin turn it
// lands on.
func setupBalanceAgentTaskFixture(t *testing.T) agentTaskFixture {
	t.Helper()
	n := nextAgentTaskFixtureN(t)
	mount := fmt.Sprintf("/atbal%d", n)

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	for _, dir := range []string{dir1, dir2} {
		if err := os.WriteFile(filepath.Join(dir, "file.bin"), []byte("agent task payload"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sid1, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount, Addition: `{"root_folder_path":"` + dir1 + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), sid1) })

	sid2, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: mount + ".balance1", Addition: `{"root_folder_path":"` + dir2 + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), sid2) })

	role := createSafeMountRole(t, n, mount)

	creator := &model.User{Username: fmt.Sprintf("atbal-user-%d", n), Role: model.Roles{int(role.ID)}}
	if err := op.CreateUser(creator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(creator.ID) })

	a := &model.Agent{AgentID: fmt.Sprintf("atbal-agent-%d", n), Name: "task-agent-bal", Enabled: true}
	// Path form, bare mount — no ".balanceN" suffix. Pre-fix, this matched
	// only whichever raw MountPath happened to equal it exactly (i.e. never
	// the .balance1 member); post-fix it must match the whole group.
	a.SetWhitelist([]string{mount})
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(a.ID) })

	return agentTaskFixture{
		mount:   mount,
		diskDir: dir1,
		srcPath: mount + "/file.bin",
		dstDir:  mount + "/dst",
		creator: creator,
		roleID:  role.ID,
		agent:   a,
	}
}

// createTask persists a task row directly (bypassing the CreateAgentTask
// HTTP handler, which most capTok-endpoint tests don't need to also exercise)
// scoped to fx's src/dst/agent/creator, in the given status.
func (fx agentTaskFixture) createTask(t *testing.T, status string) *model.AgentTask {
	t.Helper()
	task := &model.AgentTask{
		TaskID:     fmt.Sprintf("task-%d-%d", nextAgentTaskFixtureN(t), time.Now().UnixNano()),
		AgentID:    fx.agent.AgentID,
		Type:       model.AgentTaskCopy,
		SrcPath:    fx.srcPath,
		DstDir:     fx.dstDir,
		CreatorUID: fx.creator.ID,
		Status:     status,
	}
	if err := db.CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgentTask(task.ID) })
	return task
}

func (fx agentTaskFixture) issueCap(t *testing.T, task *model.AgentTask) string {
	t.Helper()
	tok, err := agentsrv.IssueTaskCap(fx.agent.AgentID, task.TaskID, task.SrcPath, task.DstDir, time.Minute)
	if err != nil {
		t.Fatalf("issue capTok: %v", err)
	}
	return tok
}

// --- driving handlers directly ---

func driveAuthedPost(u *model.User, path, body string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.User, u)
	h(c)
	return w
}

func driveCapCall(method, taskID, body string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/x", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: taskID}}
	h(c)
	return w
}

func driveContentPut(taskID, capToken, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/x", strings.NewReader(body))
	c.Request.Header.Set("Agent-Cap-Token", capToken)
	c.Params = gin.Params{{Key: "id", Value: taskID}}
	AgentTaskPutContent(c)
	return w
}

func parseAgentTaskDTO(t *testing.T, w *httptest.ResponseRecorder) AgentTaskDTO {
	t.Helper()
	var env struct {
		Data AgentTaskDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse task dto: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func parseSrcLinkResp(t *testing.T, w *httptest.ResponseRecorder) AgentTaskSrcLinkResp {
	t.Helper()
	var env struct {
		Data AgentTaskSrcLinkResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse src-link resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// --- CreateAgentTask ---

// TestCreateAgentTaskSuccess drives the success path on a non-Local driver
// (FakeRapid — see agent_task_rapid_test.go): CreateAgentTask now rejects
// Local as either src or dst (see TestCreateAgentTaskSrcIsLocalRejected /
// TestCreateAgentTaskDstIsLocalRejected below), so the "everything checks
// out" fixture can no longer be the plain Local one setupAgentTaskFixture
// mounts — that fixture is kept around for, and now exists specifically to
// exercise, the Local-rejection tests and the cap-token-endpoint tests that
// don't go through CreateAgentTask at all.
func TestCreateAgentTaskSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)

	body := fmt.Sprintf(`{"agent_id":%q,"src_path":%q,"dst_dir":%q}`, fx.agent.AgentID, fx.srcPath, fx.dstDir)
	w := driveAuthedPost(fx.creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	dto := parseAgentTaskDTO(t, w)
	if dto.Status != model.AgentTaskPending {
		t.Fatalf("new task status = %q, want pending", dto.Status)
	}
	if dto.SrcPath != fx.srcPath || dto.DstDir != fx.dstDir || dto.AgentID != fx.agent.AgentID {
		t.Fatalf("unexpected dto: %+v", dto)
	}

	row, err := db.GetAgentTaskByTaskID(dto.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgentTask(row.ID) })
	if row.CreatorUID != fx.creator.ID {
		t.Fatalf("CreatorUID = %d, want %d", row.CreatorUID, fx.creator.ID)
	}
}

func TestCreateAgentTaskUnknownAgentIs404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	body := fmt.Sprintf(`{"agent_id":"nobody-paired-this-id","src_path":%q,"dst_dir":%q}`, fx.srcPath, fx.dstDir)
	w := driveAuthedPost(fx.creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 404 {
		t.Fatalf("want 404, got %d: %s", got, w.Body.String())
	}
}

func TestCreateAgentTaskDisabledAgentIs403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	fx.agent.Enabled = false
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"agent_id":%q,"src_path":%q,"dst_dir":%q}`, fx.agent.AgentID, fx.srcPath, fx.dstDir)
	w := driveAuthedPost(fx.creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("want 403, got %d: %s", got, w.Body.String())
	}
}

// TestCreateAgentTaskCreatorNotAuthorizedIs403 pins that a creator with NO
// role at all (so no fs grant anywhere) is refused, even though the agent
// itself is perfectly valid and whitelisted — a task is never a way to
// exceed the creator's own RBAC.
func TestCreateAgentTaskCreatorNotAuthorizedIs403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	n := nextAgentTaskFixtureN(t)
	unauthorized := &model.User{Username: fmt.Sprintf("atask-noauth-%d", n)}
	if err := op.CreateUser(unauthorized); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(unauthorized.ID) })

	body := fmt.Sprintf(`{"agent_id":%q,"src_path":%q,"dst_dir":%q}`, fx.agent.AgentID, fx.srcPath, fx.dstDir)
	w := driveAuthedPost(unauthorized, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("want 403, got %d: %s", got, w.Body.String())
	}

	if _, err := db.GetAgentTaskByTaskID("should-not-exist"); err == nil {
		t.Fatal("sanity check misfired")
	}
	// A generously large limit (db.ListAgentTasks clamps to its own max
	// internally) -- this check just needs to see "every task this test
	// process could plausibly have created", not literally every row ever.
	list, err := db.ListAgentTasks("", 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list {
		if task.AgentID == fx.agent.AgentID && task.CreatorUID == unauthorized.ID {
			t.Fatalf("an unauthorized creator's task must never be created: %+v", task)
		}
	}
}

// TestCreateAgentTaskStorageNotWhitelistedIs403 pins that a fully-authorized
// creator is STILL refused if the agent's whitelist doesn't cover the
// storage — whitelist and authz are independent, both-required gates.
func TestCreateAgentTaskStorageNotWhitelistedIs403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	fx.agent.SetWhitelist(nil) // clear it
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"agent_id":%q,"src_path":%q,"dst_dir":%q}`, fx.agent.AgentID, fx.srcPath, fx.dstDir)
	w := driveAuthedPost(fx.creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("want 403, got %d: %s", got, w.Body.String())
	}
}

// TestCreateAgentTaskSrcIsLocalRejected pins that a fully-authorized,
// fully-whitelisted request is STILL refused, with a clear 400 naming
// Local, when the source resolves to a Local-driver storage: a remote agent
// cannot reach the server's local disk (see
// errs.AgentLocalDriverUnsupported's doc) — the whole point being that this
// is caught at CREATION time, never surfacing as a confusing mid-copy
// failure on the agent side. setupAgentTaskFixture mounts a genuine Local
// storage for both its src and dst, so this is the direct, no-frills case:
// both sides are Local, and the src side alone is enough to trigger the
// rejection.
func TestCreateAgentTaskSrcIsLocalRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	body := fmt.Sprintf(`{"agent_id":%q,"src_path":%q,"dst_dir":%q}`, fx.agent.AgentID, fx.srcPath, fx.dstDir)
	w := driveAuthedPost(fx.creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("want 400, got %d: %s", got, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Local") {
		t.Fatalf("error must mention Local, got: %s", w.Body.String())
	}

	list, err := db.ListAgentTasks("", 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list {
		if task.AgentID == fx.agent.AgentID && task.CreatorUID == fx.creator.ID {
			t.Fatalf("a rejected Local-src task must never be created: %+v", task)
		}
	}
}

// TestCreateAgentTaskDstIsLocalRejected isolates the DESTINATION side: a
// SOURCE on a perfectly fine non-Local storage (FakeRapid) but a
// DESTINATION resolving to Local must independently trip the same
// rejection — proving the check runs on both src AND dst, not just
// whichever happens to resolve first when both sides share one Local
// fixture (as TestCreateAgentTaskSrcIsLocalRejected's fixture does).
func TestCreateAgentTaskDstIsLocalRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	n := nextAgentTaskFixtureN(t)

	srcMount := fmt.Sprintf("/dstlocal-src%d", n)
	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "FakeRapid", MountPath: srcMount, Addition: `{"root_folder_id":"root"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), sid) })

	dstMount := fmt.Sprintf("/dstlocal-dst%d", n)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	did, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "Local", MountPath: dstMount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), did) })

	// Two safe roles (see createSafeMountRole's doc on why a single ad hoc
	// op.CreateRole call is avoided here) — a user's Role is a plain []int
	// that op.Authorize unions across, so binding both is equivalent to one
	// role scoped to both mounts.
	srcRole := createSafeMountRole(t, n, srcMount)
	dstRole := createSafeMountRole(t, nextAgentTaskFixtureN(t), dstMount)

	creator := &model.User{Username: fmt.Sprintf("dstlocal-user-%d", n), Role: model.Roles{int(srcRole.ID), int(dstRole.ID)}}
	if err := op.CreateUser(creator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(creator.ID) })

	a := &model.Agent{AgentID: fmt.Sprintf("dstlocal-agent-%d", n), Name: "dst-local-agent", Enabled: true}
	a.SetWhitelist([]string{srcMount, dstMount})
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(a.ID) })

	body := fmt.Sprintf(`{"agent_id":%q,"src_path":%q,"dst_dir":%q}`, a.AgentID, srcMount+"/src.bin", dstMount+"/dst")
	w := driveAuthedPost(creator, "/x", body, CreateAgentTask)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("want 400, got %d: %s", got, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Local") {
		t.Fatalf("error must mention Local, got: %s", w.Body.String())
	}
}

// --- heartbeat dispatch integration ---

func seedTaskCapableAgent(t *testing.T, enabled bool) (agentID string, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agentID = proto.FingerprintPubKey(pub)
	row := &model.Agent{
		AgentID: agentID,
		Name:    fmt.Sprintf("hb-task-fixture-%d", nextAgentTaskFixtureN(t)),
		PubKey:  base64.StdEncoding.EncodeToString(pub),
		Enabled: enabled,
	}
	row.SetWhitelist(nil)
	if err := db.CreateAgent(row); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(row.ID) })
	return agentID, priv
}

// TestAgentHeartbeatDispatchesClaimedTask pins the heartbeat -> claim ->
// capTok-token dispatch integration: a pending task for this agent is claimed
// and handed back with a capability token that verifies to exactly this
// task's fields.
func TestAgentHeartbeatDispatchesClaimedTask(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, priv := seedTaskCapableAgent(t, true)
	task := &model.AgentTask{
		TaskID: fmt.Sprintf("hbtask-%d", nextAgentTaskFixtureN(t)), AgentID: agentID,
		Type: model.AgentTaskCopy, SrcPath: "/hb/src", DstDir: "/hb/dst",
		Status: model.AgentTaskPending,
	}
	if err := db.CreateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgentTask(task.ID) })

	ts := time.Now().Unix()
	nonce := fmt.Sprintf("dispatch-nonce-%d", nextAgentTaskFixtureN(t))
	sig := signHeartbeat(priv, agentID, ts, nonce)
	w := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("heartbeat must succeed: %d %s", got, w.Body.String())
	}

	var env struct {
		Data AgentHeartbeatResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if env.Data.Task == nil {
		t.Fatal("expected a dispatched task in the heartbeat response")
	}
	if env.Data.Task.TaskID != task.TaskID || env.Data.Task.SrcPath != "/hb/src" || env.Data.Task.DstDir != "/hb/dst" {
		t.Fatalf("unexpected task payload: %+v", env.Data.Task)
	}
	claims, err := agentsrv.VerifyTaskCap(env.Data.Task.CapToken)
	if err != nil {
		t.Fatalf("capTok token must verify: %v", err)
	}
	if claims.TaskID != task.TaskID || claims.AgentID != agentID {
		t.Fatalf("capTok claims mismatch: %+v", claims)
	}

	reloaded, err := db.GetAgentTaskByTaskID(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskClaimed {
		t.Fatalf("task status after dispatch = %q, want claimed", reloaded.Status)
	}
}

// TestAgentHeartbeatNoDispatchWhenNothingPending pins that the Task field is
// entirely absent (nil, omitted by omitempty) when there is nothing to
// dispatch — "no task -> response unchanged".
func TestAgentHeartbeatNoDispatchWhenNothingPending(t *testing.T) {
	gin.SetMode(gin.TestMode)
	agentID, priv := seedTaskCapableAgent(t, true)

	ts := time.Now().Unix()
	nonce := fmt.Sprintf("nodispatch-nonce-%d", nextAgentTaskFixtureN(t))
	sig := signHeartbeat(priv, agentID, ts, nonce)
	w := driveAnonPost("/x", heartbeatBody(agentID, ts, nonce, sig), AgentHeartbeat)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("heartbeat must succeed: %d %s", got, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"task"`) {
		t.Fatalf("no pending task: response must omit the task field entirely, got %s", w.Body.String())
	}
}

// --- src-link ---

func TestAgentTaskSrcLinkSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSrcLinkResp(t, w)
	if resp.Size != int64(len("agent task payload")) {
		t.Fatalf("size = %d, want %d", resp.Size, len("agent task payload"))
	}
	if resp.URL == "" {
		t.Fatal("expected a non-empty URL")
	}
	// Local storage has no directly reachable URL, so this must be the
	// server-proxied /d fallback.
	if !resp.Proxied {
		t.Fatal("Local storage must be reported as Proxied")
	}
	if !strings.Contains(resp.URL, "/d") || !strings.Contains(resp.URL, "sign=") {
		t.Fatalf("proxied URL must be a signed /d link, got %q", resp.URL)
	}
}

// TestAgentTaskSrcLinkFallbackSignBoundedTTL pins FIX 3c: the proxied /d
// fallback's sign= must be bounded by the cap token's OWN remaining life,
// not the (default-unlimited) global conf.LinkExpiration common.SignPath
// implicitly used pre-fix. It mints a cap token with a short, explicit TTL
// (comfortably above minFallbackSignTTL so the floor doesn't mask the
// bound), extracts the fallback's sign= value, confirms it verifies
// immediately, then confirms it FAILS sign.Verify once that TTL has
// elapsed — pre-fix, common.SignPath's default (LinkExpiration=0) would
// have made this sign verify forever.
func TestAgentTaskSrcLinkFallbackSignBoundedTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTTL := 6 * time.Second // > minFallbackSignTTL, so the floor never engages
	capTok, err := agentsrv.IssueTaskCap(fx.agent.AgentID, task.TaskID, task.SrcPath, task.DstDir, capTTL)
	if err != nil {
		t.Fatal(err)
	}

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	resp := parseSrcLinkResp(t, w)

	parsed, err := url.Parse(resp.URL)
	if err != nil {
		t.Fatalf("parse fallback URL %q: %v", resp.URL, err)
	}
	signVal := parsed.Query().Get("sign")
	if signVal == "" {
		t.Fatalf("fallback URL missing sign=: %q", resp.URL)
	}

	// Same canonical resolution common.SignPathWithDuration and
	// server/middlewares/down.go's Down handler both use.
	ref, err := op.ResolvePathAnchor(fx.srcPath)
	if err != nil {
		t.Fatalf("resolve canonical for %q: %v", fx.srcPath, err)
	}
	canonical := ref.Canonical()

	if err := sign.Verify(canonical, signVal); err != nil {
		t.Fatalf("fresh fallback sign must verify: %v", err)
	}

	time.Sleep(capTTL + 2*time.Second)
	if err := sign.Verify(canonical, signVal); err == nil {
		t.Fatal("fallback sign must expire once the cap token's own TTL has elapsed, not linger indefinitely")
	}
}

// TestAgentTaskSrcLinkCoreProxiedFallbackRequiresAbsoluteAPIURL pins the
// site_url root-cause fix: the reverse-channel transport has no inbound HTTP
// request to derive an absolute API base from (RegisterAgentChannelHandlers
// passes common.GetApiUrl(nil), which returns conf.Conf.SiteURL verbatim, or
// "" when unset — see agent_task_core.go's doc on the proxied branch). Driving
// the core directly with apiURL="" (the exact value GetApiUrl(nil) produces
// when site_url is unconfigured) must return a clear, actionable error
// instead of ever handing back a schemeless "/d/..." link the agent's HTTP
// client cannot dial ("unsupported protocol scheme \"\"").
func TestAgentTaskSrcLinkCoreProxiedFallbackRequiresAbsoluteAPIURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, ce := agentTaskSrcLinkCore(context.Background(), "", "", task.TaskID, capTok)
	if ce == nil {
		t.Fatal("an unconfigured site_url must deny the proxied fallback, not hand back a schemeless URL")
	}
	if !strings.Contains(ce.Err.Error(), "site_url") {
		t.Fatalf("error must mention site_url so the admin knows what to configure, got %q", ce.Err.Error())
	}
}

// TestAgentTaskSrcLinkCoreProxiedFallbackUsesConfiguredAbsoluteAPIURL is the
// success-side pin: once apiURL IS absolute (site_url configured, or the HTTP
// transport's request-derived base), the proxied fallback is built against it
// verbatim.
func TestAgentTaskSrcLinkCoreProxiedFallbackUsesConfiguredAbsoluteAPIURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	resp, ce := agentTaskSrcLinkCore(context.Background(), "", "https://example.com", task.TaskID, capTok)
	if ce != nil {
		t.Fatalf("agentTaskSrcLinkCore: %+v", ce.Err)
	}
	if !resp.Proxied {
		t.Fatal("Local storage must be reported Proxied")
	}
	if !strings.HasPrefix(resp.URL, "https://example.com/d") {
		t.Fatalf("proxied URL must be absolute against the configured site_url, got %q", resp.URL)
	}
}

func TestAgentTaskSrcLinkExpiredTokenDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok, err := agentsrv.IssueTaskCap(fx.agent.AgentID, task.TaskID, task.SrcPath, task.DstDir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got != 401 {
		t.Fatalf("expired token: want 401, got %d: %s", got, w.Body.String())
	}
}

func TestAgentTaskSrcLinkTamperedTokenDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)
	tampered := capTok[:len(capTok)-1] + "x"

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, tampered), AgentTaskSrcLink)
	if got := respCode(t, w); got != 401 {
		t.Fatalf("tampered token: want 401, got %d: %s", got, w.Body.String())
	}
}

// TestAgentTaskSrcLinkCrossTaskTokenDenied pins that a token minted for task
// A is refused at task B's endpoint, even though both belong to the same
// agent and both tokens are individually valid.
func TestAgentTaskSrcLinkCrossTaskTokenDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	taskA := fx.createTask(t, model.AgentTaskClaimed)
	taskB := fx.createTask(t, model.AgentTaskClaimed)
	capA := fx.issueCap(t, taskA)

	w := driveCapCall("POST", taskB.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capA), AgentTaskSrcLink)
	if got := respCode(t, w); got != 401 {
		t.Fatalf("cross-task token: want 401, got %d: %s", got, w.Body.String())
	}
}

// TestAgentTaskSrcLinkWhitelistRemovedDenied pins the live-revocation
// property: a perfectly valid, unexpired token is refused the moment an
// admin clears the agent's whitelist, without touching the token or task at
// all.
func TestAgentTaskSrcLinkWhitelistRemovedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.agent.SetWhitelist(nil)
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("whitelist removed: want 403, got %d: %s", got, w.Body.String())
	}
}

// TestAgentTaskSrcLinkCreatorDeauthorizedDenied pins the other live-
// revocation axis: the token and whitelist are untouched, but the task's
// CREATOR no longer has read access (role stripped) — the agent must be cut
// off just the same, because it only ever acts within the creator's
// authority.
func TestAgentTaskSrcLinkCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got != 403 {
		t.Fatalf("creator de-authorized: want 403, got %d: %s", got, w.Body.String())
	}
}

func TestAgentTaskSrcLinkDisabledAgentDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.agent.Enabled = false
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got == 200 {
		t.Fatalf("disabled agent: must not succeed, got 200: %s", w.Body.String())
	}
}

func TestAgentTaskSrcLinkTerminalTaskDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskDone)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
	if got := respCode(t, w); got != 409 {
		t.Fatalf("terminal task: want 409, got %d: %s", got, w.Body.String())
	}
}

// --- balance-group whitelist (FIX 3b) ---

// TestStorageWhitelistedBalanceGroupMatchesActualMountPath pins FIX 3b at
// the storageWhitelisted unit itself: a bare-mount-path whitelist entry
// ("/atbal<n>", no ".balanceN" suffix) must match EVERY member of the
// simulated balance group across repeated calls, even though
// op.GetBalancedStorage round-robins which member each individual call
// resolves to (and, pre-fix, a suffixed member's raw MountPath never equaled
// the bare whitelist entry).
func TestStorageWhitelistedBalanceGroupMatchesActualMountPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupBalanceAgentTaskFixture(t)

	// First, confirm (independently of storageWhitelisted) that the group's
	// round-robin genuinely alternates between both members for this path —
	// op.GetBalancedStorage cycles with period 2, so two bare calls are
	// enough to observe both ids and pin that this fixture actually
	// exercises the balance-group shape the fix targets, not a degenerate
	// single-member match.
	first := op.GetBalancedStorage(fx.srcPath).GetStorage().ID
	second := op.GetBalancedStorage(fx.srcPath).GetStorage().ID
	if first == second {
		t.Fatalf("test setup problem: round-robin did not alternate between the two group members (got %d twice)", first)
	}

	// Now call storageWhitelisted alone, repeatedly: each call performs
	// exactly one op.GetBalancedStorage resolution (via
	// op.GetStorageAndActualPath), so this loop ticks through the SAME
	// period-2 cycle just confirmed above, landing on each member in turn.
	// It must return true every time — pre-fix (raw MountPath comparison
	// against a bare-mount whitelist entry), the tick landing on the
	// ".balance1" member would have failed.
	for i := 0; i < 6; i++ {
		ok, err := storageWhitelisted(fx.agent, fx.srcPath)
		if err != nil {
			t.Fatalf("iteration %d: storageWhitelisted: %v", i, err)
		}
		if !ok {
			t.Fatalf("iteration %d: whitelist %q must match every balance-group member uniformly", i, fx.mount)
		}
	}
}

// TestAgentTaskSrcLinkBalanceGroupWhitelistMatchesRegardlessOfMember drives
// the full resolveTaskCap -> storageWhitelisted -> fs.Link chain repeatedly
// against a fresh task each time, so op.GetBalancedStorage's round-robin
// state actually advances between the whitelist CHECK and the fs.Link OP it
// gates (both resolve the same virtual path independently) — the TOCTOU FIX
// 3b closes. Every call must succeed with no false 403, regardless of which
// group member the check and the op each land on.
func TestAgentTaskSrcLinkBalanceGroupWhitelistMatchesRegardlessOfMember(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupBalanceAgentTaskFixture(t)

	for i := 0; i < 6; i++ {
		task := fx.createTask(t, model.AgentTaskClaimed)
		capTok := fx.issueCap(t, task)

		w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskSrcLink)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("iteration %d: balance-group whitelist must match regardless of round-robin member, got %d: %s", i, got, w.Body.String())
		}
		resp := parseSrcLinkResp(t, w)
		if resp.Size != int64(len("agent task payload")) {
			t.Fatalf("iteration %d: size = %d, want %d", i, resp.Size, len("agent task payload"))
		}
	}
}

// TestAgentTaskPutContentBalanceGroupWhitelistMatchesRegardlessOfMember is
// the write-side mirror: the destination dir also lives on a simulated
// balance group, and the whitelist check must never diverge from whichever
// member fs.PutDirectly actually writes to.
func TestAgentTaskPutContentBalanceGroupWhitelistMatchesRegardlessOfMember(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupBalanceAgentTaskFixture(t)

	for i := 0; i < 6; i++ {
		task := fx.createTask(t, model.AgentTaskClaimed)
		capTok := fx.issueCap(t, task)

		body := fmt.Sprintf("copied bytes %d", i)
		w := driveContentPut(task.TaskID, capTok, body)
		if got := respCode(t, w); got != 200 {
			t.Fatalf("iteration %d: balance-group whitelist must match regardless of round-robin member, got %d: %s", i, got, w.Body.String())
		}
	}
}

// --- content (write) ---

func TestAgentTaskPutContentSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveContentPut(task.TaskID, capTok, "copied bytes")
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}

	// The destination filename is derived from the token's Src field
	// (stdpath.Base), never client input — see AgentTaskPutContent's doc.
	written, err := os.ReadFile(filepath.Join(fx.diskDir, "dst", "file.bin"))
	if err != nil {
		t.Fatalf("read back written file: %v", err)
	}
	if string(written) != "copied bytes" {
		t.Fatalf("written content = %q, want %q", written, "copied bytes")
	}
}

// TestAgentTaskPutContentWhitelistRemovedDenied mirrors the src-link
// whitelist-revocation test on the write side.
func TestAgentTaskPutContentWhitelistRemovedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.agent.SetWhitelist(nil)
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	w := driveContentPut(task.TaskID, capTok, "should not land")
	if got := respCode(t, w); got != 403 {
		t.Fatalf("whitelist removed: want 403, got %d: %s", got, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(fx.diskDir, "dst", "file.bin")); err == nil {
		t.Fatal("a denied write must not land on disk")
	}
}

// TestAgentTaskPutContentCreatorDeauthorizedDenied mirrors the src-link
// creator-deauthorization test on the write side.
func TestAgentTaskPutContentCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	w := driveContentPut(task.TaskID, capTok, "should not land")
	if got := respCode(t, w); got != 403 {
		t.Fatalf("creator de-authorized: want 403, got %d: %s", got, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(fx.diskDir, "dst", "file.bin")); err == nil {
		t.Fatal("a denied write must not land on disk")
	}
}

func TestAgentTaskPutContentTerminalTaskDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskFailed)
	capTok := fx.issueCap(t, task)

	w := driveContentPut(task.TaskID, capTok, "should not land")
	if got := respCode(t, w); got != 409 {
		t.Fatalf("terminal task: want 409, got %d: %s", got, w.Body.String())
	}
}

// --- progress / status ---

func TestAgentTaskProgressSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q,"progress":42,"detail":"working"}`, capTok), AgentTaskProgress)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	dto := parseAgentTaskDTO(t, w)
	if dto.Progress != 42 || dto.Detail != "working" {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	if dto.Status != model.AgentTaskRunning {
		t.Fatalf("first progress report must flip claimed -> running, got %q", dto.Status)
	}
}

func TestAgentTaskProgressClampedToRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q,"progress":999}`, capTok), AgentTaskProgress)
	dto := parseAgentTaskDTO(t, w)
	if dto.Progress != 100 {
		t.Fatalf("progress must clamp to 100, got %d", dto.Progress)
	}
}

func TestAgentTaskProgressOnTerminalTaskIsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskDone)
	task.Progress = 100
	task.Detail = "already finished"
	if err := db.UpdateAgentTask(task); err != nil {
		t.Fatal(err)
	}
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q,"progress":5,"detail":"trying to reopen"}`, capTok), AgentTaskProgress)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("terminal progress call must still respond 200 (idempotent no-op), got %d: %s", got, w.Body.String())
	}
	dto := parseAgentTaskDTO(t, w)
	if dto.Progress != 100 || dto.Detail != "already finished" {
		t.Fatalf("a terminal task's state must be frozen, got %+v", dto)
	}
}

func TestAgentTaskStatusDoneThenFailedIsFrozen(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w1 := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q,"status":"done","detail":"ok"}`, capTok), AgentTaskStatus)
	if got := respCode(t, w1); got != 200 {
		t.Fatalf("first finalize: want 200, got %d: %s", got, w1.Body.String())
	}
	dto1 := parseAgentTaskDTO(t, w1)
	if dto1.Status != model.AgentTaskDone || dto1.Progress != 100 {
		t.Fatalf("unexpected first finalize: %+v", dto1)
	}

	// A late/retried "failed" must NOT flip an already-done task.
	w2 := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q,"status":"failed","detail":"late failure"}`, capTok), AgentTaskStatus)
	if got := respCode(t, w2); got != 200 {
		t.Fatalf("idempotent re-finalize must still respond 200, got %d: %s", got, w2.Body.String())
	}
	dto2 := parseAgentTaskDTO(t, w2)
	if dto2.Status != model.AgentTaskDone {
		t.Fatalf("a done task must never flip to failed, got %q", dto2.Status)
	}

	reloaded, err := db.GetAgentTaskByTaskID(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != model.AgentTaskDone {
		t.Fatalf("persisted status must stay done, got %q", reloaded.Status)
	}
}

func TestAgentTaskStatusInvalidValueRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q,"status":"bogus"}`, capTok), AgentTaskStatus)
	if got := respCode(t, w); got != 400 {
		t.Fatalf("invalid status: want 400, got %d: %s", got, w.Body.String())
	}
}

// --- AdminListAgentTasks (design W12-4: task visibility) ---

// driveAdminAgentTaskList runs AdminListAgentTasks with the given raw query
// string (e.g. "agent_id=x&limit=2"). Admin-gating itself is structural
// (middlewares.RequireAdmin at the router, see server/router_v4.go) and
// exercised end to end in server/router_v4_test.go, same convention as
// every other admin handler test in this package (see agent_test.go's
// top-of-file doc) — this drives the handler directly.
func driveAdminAgentTaskList(rawQuery string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	target := "/x"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	c.Request = httptest.NewRequest("GET", target, nil)
	AdminListAgentTasks(c)
	return w
}

func parseAgentTaskDTOList(t *testing.T, w *httptest.ResponseRecorder) []AgentTaskDTO {
	t.Helper()
	var env struct {
		Data []AgentTaskDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse agent task list: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// TestAdminListAgentTasksFilterAndOrder pins the two properties the router
// test can't (query parsing + wiring to db.ListAgentTasks): filtering by
// agent_id, and newest-first ordering, through the real handler.
func TestAdminListAgentTasksFilterAndOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	t1 := fx.createTask(t, model.AgentTaskPending)
	t2 := fx.createTask(t, model.AgentTaskDone)

	// A task on a SECOND, unrelated agent -- must not leak into fx's filter.
	other := &model.Agent{AgentID: fmt.Sprintf("atask-other-%d", nextAgentTaskFixtureN(t)), Enabled: true}
	if err := db.CreateAgent(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(other.ID) })
	otherTask := &model.AgentTask{TaskID: fmt.Sprintf("other-task-%d", time.Now().UnixNano()), AgentID: other.AgentID, Status: model.AgentTaskPending}
	if err := db.CreateAgentTask(otherTask); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgentTask(otherTask.ID) })

	w := driveAdminAgentTaskList("agent_id=" + fx.agent.AgentID)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	list := parseAgentTaskDTOList(t, w)
	if len(list) != 2 {
		t.Fatalf("expected exactly fx's 2 tasks, got %d: %+v", len(list), list)
	}
	// Newest-first: t2 (created second) must lead t1.
	if list[0].TaskID != t2.TaskID || list[1].TaskID != t1.TaskID {
		t.Fatalf("expected newest-first [t2 t1], got [%s %s]", list[0].TaskID, list[1].TaskID)
	}
	for _, dto := range list {
		if dto.AgentID != fx.agent.AgentID {
			t.Fatalf("filter leaked another agent's task: %+v", dto)
		}
		if dto.ID == 0 {
			t.Fatalf("DTO must expose a non-zero id: %+v", dto)
		}
	}
}

// TestAdminListAgentTasksUnfilteredIncludesEveryAgent pins that an empty
// ?agent_id= means "every agent", not "no agent".
func TestAdminListAgentTasksUnfilteredIncludesEveryAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskRunning)

	w := driveAdminAgentTaskList("")
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	list := parseAgentTaskDTOList(t, w)
	found := false
	for _, dto := range list {
		if dto.TaskID == task.TaskID {
			found = true
			if dto.Status != model.AgentTaskRunning {
				t.Fatalf("unexpected status in list: %+v", dto)
			}
		}
	}
	if !found {
		t.Fatalf("unfiltered list must include fx's task, got %+v", list)
	}
}

// TestAdminListAgentTasksPaging pins ?limit= actually bounds the page.
func TestAdminListAgentTasksPaging(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	fx.createTask(t, model.AgentTaskPending)
	fx.createTask(t, model.AgentTaskPending)
	fx.createTask(t, model.AgentTaskPending)

	w := driveAdminAgentTaskList(fmt.Sprintf("agent_id=%s&limit=1", fx.agent.AgentID))
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	list := parseAgentTaskDTOList(t, w)
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 row with limit=1, got %d: %+v", len(list), list)
	}
}
