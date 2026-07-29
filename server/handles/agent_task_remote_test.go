package handles

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	local "github.com/alist-org/alist/v3/drivers/local"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
)

// fakeRemoteDriver (design: CreateAgentTask's Local-driver rejection, see
// errs.AgentLocalDriverUnsupported) is a test-only driver standing in for a
// genuine, non-Local cloud drive: it is what TestAgentDelegatedCopyEndToEnd
// switches to once Local-to-Local is no longer a creatable agent task, so
// that test keeps proving the WHOLE reverse-agent pipeline works end to end
// — pair, create, heartbeat-dispatch, download, upload, progress, status —
// for the shape of copy the feature actually supports, rather than losing
// that coverage entirely.
//
// It delegates every real filesystem operation to an embedded *local.Local
// (List/Get/MakeDir/Put/Move/Remove/... all behave exactly like production
// Local, including real bytes landing on real disk, so an end-to-end test's
// on-disk assertions still work unchanged) and overrides exactly two things:
//
//   - Config(): a different Name ("FakeRemote", not "Local") — the literal
//     string CreateAgentTask's rejection keys on — and, critically, no
//     OnlyLocal, so driver.Config.MustProxy() is false and
//     AgentTaskSrcLink's direct-URL branch (agent_task_core.go) is the one
//     actually exercised, not Local's forced signed-/d proxy fallback.
//   - Link(): instead of Local's os.Open (an in-process MFile with no URL
//     at all), it serves the file over a real, in-process httptest.Server
//     and returns THAT URL — a genuine, agent-fetchable link, standing in
//     for what a real cloud drive's direct download URL would be.
func init() {
	op.RegisterDriver(func() driver.Driver { return &fakeRemoteDriver{} })
}

type fakeRemoteDriver struct {
	local.Local
	root string
	srv  *httptest.Server
}

func (d *fakeRemoteDriver) Config() driver.Config {
	return driver.Config{Name: "FakeRemote", LocalSort: true, NoCache: true, DefaultRoot: "/"}
}

func (d *fakeRemoteDriver) Init(ctx context.Context) error {
	if err := d.Local.Init(ctx); err != nil {
		return err
	}
	d.root = d.GetRootPath()
	d.srv = httptest.NewServer(http.FileServer(http.Dir(d.root)))
	return nil
}

func (d *fakeRemoteDriver) Drop(ctx context.Context) error {
	if d.srv != nil {
		d.srv.Close()
	}
	return d.Local.Drop(ctx)
}

// Link ignores args.Type (no thumbnail support needed here) and just maps
// the object's real on-disk path to a URL under the driver's own httptest
// server — the file bytes served ARE the real bytes on real disk, so a
// download through this URL is byte-for-byte what a direct-from-drive
// download of a real cloud file would look like.
func (d *fakeRemoteDriver) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	rel, err := filepath.Rel(d.root, file.GetPath())
	if err != nil {
		return nil, err
	}
	return &model.Link{URL: d.srv.URL + "/" + filepath.ToSlash(rel)}, nil
}

var _ driver.Driver = (*fakeRemoteDriver)(nil)

// setupRemoteAgentTaskFixture mirrors setupAgentTaskFixture (agent_task_test.go)
// field-for-field — same shape (agentTaskFixture), same on-disk-verifiable
// diskDir, same single-mount-covers-both-src-and-dst layout — but mounts
// fakeRemoteDriver instead of Local, for the one test
// (TestAgentDelegatedCopyEndToEnd) that needs a fixture CreateAgentTask
// will actually accept post-Local-rejection while still proving real bytes
// move on real disk.
func setupRemoteAgentTaskFixture(t *testing.T) agentTaskFixture {
	t.Helper()
	n := nextAgentTaskFixtureN(t)
	mount := fmt.Sprintf("/arem%d", n)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.bin"), []byte("agent task payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}

	sid, err := op.CreateStorage(t.Context(), model.Storage{
		Driver: "FakeRemote", MountPath: mount, Addition: `{"root_folder_path":"` + dir + `"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteStorageById(t.Context(), sid) })

	role := createSafeMountRole(t, n, mount)

	creator := &model.User{Username: fmt.Sprintf("arem-user-%d", n), Role: model.Roles{int(role.ID)}}
	if err := op.CreateUser(creator); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.DeleteUserById(creator.ID) })

	a := &model.Agent{AgentID: fmt.Sprintf("arem-agent-%d", n), Name: "remote-task-agent", Enabled: true}
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
