package handles

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/gin-gonic/gin"
)

// Rapid-upload ("秒传") cap-op tests (design SP-H / H1). The property under test
// is the SAME "capability is not a bypass" invariant the src-link / put-content
// cores enforce: the new agent.task.rapid op must re-verify the token through
// resolveTaskCapCore AND run BOTH authorization gates — ActRead on the source
// (reading the src hash is a src read) and ActWrite on the destination — under
// the task creator's LIVE identity, plus the storage whitelist, before it ever
// attempts a rapid upload. On any miss/error it must fall back (rapid=false)
// WITHOUT failing the task; the destination filename is always derived from the
// token's Src, never caller input (the wire request carries no filename field).

// --- a registered fake driver that supports PutRapid + exposes a hashed src ---
//
// Local (the standard fixture's driver) neither implements PutRapid nor exposes
// a whole-file hash, so it can only ever drive the fall-back path. To exercise a
// genuine rapid HIT through the whole core (both gates -> op.PutRapid -> success
// audit), this fake driver exposes a source file WITH an MD5 hash and a dst dir,
// and reports a rapid hit — recording the name it was asked to register so the
// test can prove the filename came from the token's Src, not caller input.

func init() {
	op.RegisterDriver(func() driver.Driver { return &fakeRapidDriver{} })
}

type fakeRapidAddition struct {
	driver.RootID
}

type fakeRapidDriver struct {
	model.Storage
	fakeRapidAddition
}

// lastRapidName records the file name the most recent TryRapidUpload was asked
// to register, for the filename-from-token assertion.
var lastRapidName string

func (d *fakeRapidDriver) Config() driver.Config {
	return driver.Config{Name: "FakeRapid", LocalSort: true, NoCache: true}
}
func (d *fakeRapidDriver) GetStorage() *model.Storage     { return &d.Storage }
func (d *fakeRapidDriver) SetStorage(s model.Storage)     { d.Storage = s }
func (d *fakeRapidDriver) GetAddition() driver.Additional { return &d.fakeRapidAddition }
func (d *fakeRapidDriver) Init(ctx context.Context) error { return nil }
func (d *fakeRapidDriver) Drop(ctx context.Context) error { return nil }

func (d *fakeRapidDriver) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	if dir.GetID() == "dst" {
		return nil, nil
	}
	return []model.Obj{
		&model.Object{ID: "src", Name: "src.bin", Size: 18, IsFolder: false, HashInfo: utils.NewHashInfo(utils.MD5, "0123456789abcdef0123456789abcdef")},
		&model.Object{ID: "dst", Name: "dst", Size: 0, IsFolder: true},
	}, nil
}

func (d *fakeRapidDriver) Get(ctx context.Context, path string) (model.Obj, error) {
	switch path {
	case "/src.bin":
		return &model.Object{ID: "src", Path: path, Name: "src.bin", Size: 18, HashInfo: utils.NewHashInfo(utils.MD5, "0123456789abcdef0123456789abcdef")}, nil
	case "/dst":
		return &model.Object{ID: "dst", Path: path, Name: "dst", IsFolder: true}, nil
	}
	return &model.Object{ID: "root", Path: path, Name: "root", IsFolder: true}, nil
}

func (d *fakeRapidDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	return &model.Link{URL: "https://drive.example/" + file.GetID()}, nil
}

func (d *fakeRapidDriver) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	return nil
}

func (d *fakeRapidDriver) TryRapidUpload(ctx context.Context, dstDir model.Obj, name string, size int64, hashes utils.HashInfo) (model.Obj, bool, error) {
	lastRapidName = name
	return &model.Object{ID: "reused", Name: name, Size: size, HashInfo: hashes}, true, nil
}

var _ driver.Driver = (*fakeRapidDriver)(nil)
var _ driver.PutRapid = (*fakeRapidDriver)(nil)

// rapidFixture mounts the FakeRapid driver and wires a creator + whitelisted
// agent, mirroring setupAgentTaskFixture but for the driver that can actually
// report a rapid hit.
type rapidFixture struct {
	mount   string
	srcPath string
	dstDir  string
	creator *model.User
	agent   *model.Agent
}

func setupRapidFixture(t *testing.T) rapidFixture {
	t.Helper()
	n := nextAgentTaskFixtureN(t)
	mount := fmt.Sprintf("/frm%d", n)

	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "FakeRapid", MountPath: mount, Addition: `{"root_folder_id":"root"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), sid) })

	role := createSafeMountRole(t, n, mount)
	creator := &model.User{Username: fmt.Sprintf("frm-user-%d", n), Role: model.Roles{int(role.ID)}}
	if err := op.CreateUser(creator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(creator.ID) })

	a := &model.Agent{AgentID: fmt.Sprintf("frm-agent-%d", n), Name: "rapid-agent", Enabled: true}
	a.SetWhitelist([]string{mount})
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAgent(a.ID) })

	return rapidFixture{mount: mount, srcPath: mount + "/src.bin", dstDir: mount + "/dst", creator: creator, agent: a}
}

func (fx rapidFixture) createTask(t *testing.T, status string) *model.AgentTask {
	t.Helper()
	task := &model.AgentTask{
		TaskID:     fmt.Sprintf("rtask-%d", nextAgentTaskFixtureN(t)),
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

func (fx rapidFixture) issueCap(t *testing.T, task *model.AgentTask) string {
	t.Helper()
	tok, err := agentsrv.IssueTaskCap(fx.agent.AgentID, task.TaskID, task.SrcPath, task.DstDir, time.Minute)
	if err != nil {
		t.Fatalf("issue capTok: %v", err)
	}
	return tok
}

func parseRapidResp(t *testing.T, w *httptest.ResponseRecorder) AgentTaskTryRapidResp {
	t.Helper()
	var env struct {
		Data AgentTaskTryRapidResp `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse rapid resp: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

// --- HTTP transport round trip ---

// TestAgentTaskTryRapidHTTPRoundTrip drives the actual gin handler end-to-end:
// an authorized rapid request over HTTP returns 200 rapid=true.
func TestAgentTaskTryRapidHTTPRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	w := driveCapCall("POST", task.TaskID, fmt.Sprintf(`{"cap_token":%q}`, capTok), AgentTaskTryRapid)
	if got := respCode(t, w); got != 200 {
		t.Fatalf("want 200, got %d: %s", got, w.Body.String())
	}
	if !parseRapidResp(t, w).Rapid {
		t.Fatal("expected rapid=true over HTTP")
	}
}

// --- reverse-channel transport round trip + live re-auth ---

// TestChannelTryRapidRoundTrip proves the rapid op works over the mutual-authed
// reverse channel, funnelling through the SAME core as HTTP.
func TestChannelTryRapidRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	rapid, err := cc.TryRapid(testCtx(t), task.TaskID, capTok)
	if err != nil {
		t.Fatalf("channel TryRapid: %v", err)
	}
	if !rapid {
		t.Fatal("expected rapid hit over the channel")
	}
}

// TestChannelTryRapidCreatorDeauthorizedDenied pins live re-auth over the
// channel: stripping the creator's role denies the very next rapid op — the
// channel carries no standing authorization.
func TestChannelTryRapidCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	if _, err := cc.TryRapid(testCtx(t), task.TaskID, capTok); err == nil {
		t.Fatal("de-authorized creator must deny the channel rapid op")
	}
}

// --- rapid HIT: authorized + hashed source -> rapid=true, filename from token ---

func TestAgentTaskTryRapidHit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)
	lastRapidName = ""

	rapid, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, capTok)
	if ce != nil {
		t.Fatalf("authorized rapid must not error: %+v", ce.Err)
	}
	if !rapid {
		t.Fatal("expected rapid hit=true")
	}
	if lastRapidName != "src.bin" {
		t.Fatalf("rapid filename = %q, want src.bin (derived from token Src, never caller input)", lastRapidName)
	}
}

// --- fall-back: a source with no usable hash (Local) -> rapid=false, no fail ---

func TestAgentTaskTryRapidNoSrcHashFallsBack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	rapid, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, capTok)
	if ce != nil {
		t.Fatalf("a no-hash source must fall back, not error: %+v", ce.Err)
	}
	if rapid {
		t.Fatal("a source with no usable hash must yield rapid=false (fall back to streaming)")
	}
}

// --- re-auth: BOTH gates + choke point enforced ---

// TestAgentTaskTryRapidWhitelistRemovedDenied: clearing the whitelist denies the
// rapid op (whitelist re-checked live, same as src-link/put-content).
func TestAgentTaskTryRapidWhitelistRemovedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.agent.SetWhitelist(nil)
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	_, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 403 {
		t.Fatalf("cleared whitelist must deny with 403, got %+v", ce)
	}
}

// TestAgentTaskTryRapidCreatorDeauthorizedDenied: stripping the creator's role
// denies the rapid op — op.Authorize runs under the creator's LIVE identity.
func TestAgentTaskTryRapidCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	_, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 403 {
		t.Fatalf("de-authorized creator must deny with 403, got %+v", ce)
	}
}

// TestAgentTaskTryRapidDstWriteGateEnforced isolates the DESTINATION-write gate:
// a creator scoped to READ the whole mount (implicit read on any scope-covered
// path) but with NO write bit passes the source-read gate yet must be denied at
// the dst-write gate — proving the rapid core runs BOTH authorizations, not just
// the read one.
func TestAgentTaskTryRapidDstWriteGateEnforced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)

	// Rebind the creator to a read-only scope over the mount (Permission 0 =>
	// no write bit; ActRead is implicit for any scope-covered path).
	roRole := &model.Role{
		Name:             fmt.Sprintf("frm-ro-%d", nextAgentTaskFixtureN(t)),
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

	_, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 403 {
		t.Fatalf("a creator who may read src but not write dst must be denied at the dst-write gate (403), got %+v", ce)
	}
}

// TestAgentTaskTryRapidTerminalTaskDenied: a settled task rejects the rapid op
// (409), same terminal guard as the other cap ops.
func TestAgentTaskTryRapidTerminalTaskDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskDone)
	capTok := fx.issueCap(t, task)

	_, ce := agentTaskTryRapidCore(context.Background(), "", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 409 {
		t.Fatalf("terminal task must reject with 409, got %+v", ce)
	}
}

// TestAgentTaskTryRapidBoundAgentMismatchDenied: a valid token presented with a
// boundAgentID (channel identity) naming a DIFFERENT agent is refused up front
// (401) — the same channel defense-in-depth resolveTaskCapCore applies to every
// cap op.
func TestAgentTaskTryRapidBoundAgentMismatchDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupRapidFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, ce := agentTaskTryRapidCore(context.Background(), "some-other-agent", task.TaskID, capTok)
	if ce == nil || ce.HTTPCode != 401 {
		t.Fatalf("a token redeemed over the wrong agent's channel must be refused (401), got %+v", ce)
	}
}
