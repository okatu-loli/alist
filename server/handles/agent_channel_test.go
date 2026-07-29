package handles

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/AlistGo/alist-agent/proto"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/AlistGo/alist-agent/agent"
	"github.com/AlistGo/alist-agent/channel"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/server/middlewares"
	"github.com/gin-gonic/gin"
)

// Reverse-channel cap-op tests (design R3). The overriding property under test
// is the R2 review's non-negotiable one: the mutual-authed, AEAD-encrypted
// channel is TRANSPORT, NOT AUTHORIZATION. Every cap op sent over the channel
// must re-run the SAME live re-authorization the HTTP path does
// (resolveTaskCapCore + whitelist + op.Authorize under the creator's live
// identity + Enabled), so a disabled agent / cleared whitelist / de-authorized
// creator denies the very next cap op over the channel — the channel's standing
// handshake trust grants nothing on its own.

func genEd25519(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	return pub, priv
}

// liveReverseChannel dials a real loopback agent listener and returns both
// live channel ends, with the SERVER-side cap handlers already registered and
// bound to agentID (so the channel's proven identity is agentID). It mirrors
// exactly what R4 will wire in production: the server dials, the agent listens,
// the handshake proves both, and the server registers the domain handlers on
// the live channel.
func liveReverseChannel(t *testing.T, agentID string) (server, agent *channel.Channel) {
	t.Helper()
	agentPub, agentPriv := genEd25519(t)
	serverPub, serverPriv := genEd25519(t)

	got := make(chan *channel.Channel, 1)
	l, err := channel.NewListener(channel.ListenerConfig{
		AgentID:         agentID,
		AgentPriv:       agentPriv,
		PinnedServerPub: serverPub,
		OnChannel:       func(ch *channel.Channel) { got <- ch },
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err = channel.DialAgent(ctx, wsURL, serverPriv, agentPub, agentID)
	if err != nil {
		t.Fatalf("DialAgent: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	select {
	case agent = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("agent side never received a live channel")
	}
	t.Cleanup(func() { _ = agent.Close() })

	// The server registers the domain cap handlers on its live channel end,
	// bound to the identity the handshake proved.
	RegisterAgentChannelHandlers(server, agentID)
	return server, agent
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// --- src-link over the channel + live re-auth ---

func TestChannelSrcLinkRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	// The channel src-link's proxied fallback needs an ABSOLUTE api base (see
	// agent_task_core.go's doc on the site_url root-cause fix) — this test
	// never actually dials the URL, so a placeholder absolute value is enough;
	// only its scheme matters here.
	oldSiteURL := conf.Conf.SiteURL
	conf.Conf.SiteURL = "http://alist-test.invalid"
	t.Cleanup(func() { conf.Conf.SiteURL = oldSiteURL })

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	link, err := cc.SrcLink(testCtx(t), task.TaskID, capTok)
	if err != nil {
		t.Fatalf("channel SrcLink: %v", err)
	}
	if link.Size != int64(len("agent task payload")) {
		t.Fatalf("size = %d, want %d", link.Size, len("agent task payload"))
	}
	if !link.Proxied {
		t.Fatal("Local storage must be reported Proxied over the channel too")
	}
}

// TestChannelSrcLinkWhitelistRemovedDenied pins live re-auth over the channel:
// clearing the whitelist denies the very next channel cap op, proving the
// channel does not carry standing authorization.
func TestChannelSrcLinkWhitelistRemovedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	fx.agent.SetWhitelist(nil)
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	if _, err := cc.SrcLink(testCtx(t), task.TaskID, capTok); err == nil {
		t.Fatal("cleared whitelist must deny the channel src-link (live re-auth, not standing trust)")
	}
}

// TestChannelSrcLinkDisabledAgentDenied pins that disabling the agent denies
// the next channel cap op — the same live revocation the HTTP path enforces.
func TestChannelSrcLinkDisabledAgentDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	fx.agent.Enabled = false
	if err := db.UpdateAgent(fx.agent); err != nil {
		t.Fatal(err)
	}

	if _, err := cc.SrcLink(testCtx(t), task.TaskID, capTok); err == nil {
		t.Fatal("disabled agent must deny the channel src-link")
	}
}

// TestChannelSrcLinkCreatorDeauthorizedDenied pins the creator-authority axis:
// stripping the creator's role denies the channel cap op, because the agent
// only ever acts within the creator's live authority.
func TestChannelSrcLinkCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	if _, err := cc.SrcLink(testCtx(t), task.TaskID, capTok); err == nil {
		t.Fatal("de-authorized creator must deny the channel src-link")
	}
}

// TestChannelCrossAgentTokenDenied pins the channel-identity defense-in-depth:
// a valid cap token minted for a DIFFERENT agent is refused over this agent's
// channel, even though the token itself verifies — the token may only be
// redeemed over the channel of the agent it names. (Defense in depth ON TOP of
// the token, never a substitute: the token still verifies here, but the bound
// channel identity denies it.)
func TestChannelCrossAgentTokenDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fxA := setupAgentTaskFixture(t)
	fxB := setupAgentTaskFixture(t)
	// A task + valid cap for agent B.
	taskB := fxB.createTask(t, model.AgentTaskClaimed)
	capB := fxB.issueCap(t, taskB)

	// A live channel whose proven identity is agent A.
	_, agent := liveReverseChannel(t, fxA.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	// Presenting agent B's (individually valid) token over agent A's channel
	// must be refused.
	if _, err := cc.SrcLink(testCtx(t), taskB.TaskID, capB); err == nil {
		t.Fatal("a cap token for a different agent must be refused over this channel")
	}
}

// --- progress / status over the channel ---

func TestChannelProgressAndStatusRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)
	ctx := testCtx(t)

	if err := cc.Progress(ctx, task.TaskID, capTok, 42, "half"); err != nil {
		t.Fatalf("channel Progress: %v", err)
	}
	// First progress flips claimed -> running, persisted server-side through
	// the shared core.
	row, err := db.GetAgentTaskByTaskID(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != model.AgentTaskRunning || row.Progress != 42 {
		t.Fatalf("after channel progress: status=%q progress=%d, want running/42", row.Status, row.Progress)
	}

	if err := cc.Status(ctx, task.TaskID, capTok, proto.TaskStatusDone, "ok"); err != nil {
		t.Fatalf("channel Status: %v", err)
	}
	row, err = db.GetAgentTaskByTaskID(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != model.AgentTaskDone || row.Progress != 100 {
		t.Fatalf("after channel status done: %+v", row)
	}
}

// --- streaming put-content over the channel ---

func TestChannelPutContentStreamsToDst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	// A payload spanning several chunks, to exercise the multi-chunk streaming
	// path (not a single-frame degenerate case).
	payload := strings.Repeat("reverse-channel-bytes ", 200_000) // ~4.4 MiB > MaxChannelChunkBytes
	if err := cc.PutContent(testCtx(t), task.TaskID, capTok, strings.NewReader(payload), int64(len(payload)), nil); err != nil {
		t.Fatalf("channel PutContent: %v", err)
	}

	// The destination filename is derived from the token's Src (stdpath.Base),
	// never client input — same as the HTTP path.
	written, err := os.ReadFile(filepath.Join(fx.diskDir, "dst", "file.bin"))
	if err != nil {
		t.Fatalf("read back written file: %v", err)
	}
	if string(written) != payload {
		t.Fatalf("channel-streamed content mismatch: got %d bytes, want %d", len(written), len(payload))
	}
}

// TestChannelPutContentCreatorDeauthorizedDenied pins that a write whose
// creator was de-authorized before the op is denied over the channel, and no
// bytes land on disk — begin refuses synchronously, so op.Put never runs.
func TestChannelPutContentCreatorDeauthorizedDenied(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	cc := agentpkg.NewChannelCapClient(agent)

	fx.creator.Role = model.Roles{}
	if err := op.UpdateUser(fx.creator); err != nil {
		t.Fatal(err)
	}

	err := cc.PutContent(testCtx(t), task.TaskID, capTok, strings.NewReader("should not land"), int64(len("should not land")), nil)
	if err == nil {
		t.Fatal("de-authorized creator must deny the channel put-content")
	}
	if _, statErr := os.Stat(filepath.Join(fx.diskDir, "dst", "file.bin")); statErr == nil {
		t.Fatal("a denied channel write must not land on disk")
	}
}

// TestChannelPutContentOverCapRejected drives the raw stream protocol to prove
// the hard byte cap engages: with maxAgentStreamBytes lowered, a stream whose
// chunks exceed the cap is rejected mid-stream (before the excess is written),
// even though begin was authorized.
func TestChannelPutContentOverCapRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	// Lower the hard cap for this test only.
	oldCap := maxAgentStreamBytes
	maxAgentStreamBytes = 8
	t.Cleanup(func() { maxAgentStreamBytes = oldCap })

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	ctx := testCtx(t)

	// Declare a large size so the per-request size bound doesn't trip first —
	// we want the HARD cap (maxAgentStreamBytes) to be what rejects.
	streamID := "overcap-stream"
	beginPayload, _ := json.Marshal(proto.ChannelPutBeginReq{TaskID: task.TaskID, CapToken: capTok, StreamID: streamID, Size: 1 << 20})
	if _, err := agent.Send(ctx, proto.MethodTaskPutBegin, beginPayload); err != nil {
		t.Fatalf("begin (authorized) must succeed: %v", err)
	}

	// First chunk (8 bytes) is exactly the cap and is accepted.
	chunk0, _ := json.Marshal(proto.ChannelPutChunkReq{StreamID: streamID, Seq: 0, Data: []byte("12345678")})
	if _, err := agent.Send(ctx, proto.MethodTaskPutChunk, chunk0); err != nil {
		t.Fatalf("first chunk within cap must be accepted: %v", err)
	}
	// Second chunk pushes the running total over the cap → must be rejected.
	chunk1, _ := json.Marshal(proto.ChannelPutChunkReq{StreamID: streamID, Seq: 1, Data: []byte("9")})
	if _, err := agent.Send(ctx, proto.MethodTaskPutChunk, chunk1); err == nil {
		t.Fatal("a chunk pushing the stream over the hard byte cap must be rejected")
	}
}

// TestChannelPutContentOutOfOrderChunkRejected pins the sequence guard
// (defense in depth atop the channel's own in-order AEAD delivery).
func TestChannelPutContentOutOfOrderChunkRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)
	task := fx.createTask(t, model.AgentTaskClaimed)
	capTok := fx.issueCap(t, task)

	_, agent := liveReverseChannel(t, fx.agent.AgentID)
	ctx := testCtx(t)

	streamID := "ooo-stream"
	beginPayload, _ := json.Marshal(proto.ChannelPutBeginReq{TaskID: task.TaskID, CapToken: capTok, StreamID: streamID, Size: 1 << 20})
	if _, err := agent.Send(ctx, proto.MethodTaskPutBegin, beginPayload); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Seq 5 when 0 is expected.
	chunk, _ := json.Marshal(proto.ChannelPutChunkReq{StreamID: streamID, Seq: 5, Data: []byte("x")})
	if _, err := agent.Send(ctx, proto.MethodTaskPutChunk, chunk); err == nil {
		t.Fatal("an out-of-order chunk seq must be rejected")
	}
}

// --- reverse end-to-end: dispatch push -> agent runs -> dst written ---

// TestAgentReverseDelegatedCopyEndToEnd is the reverse-transport mirror of
// TestAgentDelegatedCopyEndToEnd: the server dials the agent's loopback
// listener, PUSHES a claimed task over the channel, and the agent runs the
// whole copy — status, src-link, streaming put, status — back over the SAME
// channel into the shared cores, ending with the destination bytes on disk and
// the task marked done.
func TestAgentReverseDelegatedCopyEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fx := setupAgentTaskFixture(t)

	// The channel src-link falls back to this server's signed /d route; the
	// agent must be able to reach it, so serve /d over a loopback HTTP server
	// and point SiteURL at it (common.GetApiUrl(nil), which the channel
	// src-link core uses, resolves to SiteURL).
	r := gin.New()
	r.GET("/d/*path", middlewares.Down(), Down)
	dsrv := httptest.NewServer(r)
	defer dsrv.Close()
	oldSiteURL := conf.Conf.SiteURL
	conf.Conf.SiteURL = dsrv.URL
	t.Cleanup(func() { conf.Conf.SiteURL = oldSiteURL })

	// A pending task for this agent.
	task := fx.createTask(t, model.AgentTaskPending)

	// Live reverse channel; the agent registers its dispatch handler.
	server, agent := liveReverseChannel(t, fx.agent.AgentID)
	done := make(chan error, 1)
	ra := agentpkg.NewReverseAgent(agent)
	ra.OnTaskDone = func(_ string, err error) { done <- err }

	// The server pushes the claimed task (+ fresh cap token) — the reverse
	// equivalent of the heartbeat handing back a task.
	dispatched, err := DispatchAgentTaskOverChannel(testCtx(t), server, fx.agent.AgentID)
	if err != nil {
		t.Fatalf("DispatchAgentTaskOverChannel: %v", err)
	}
	if !dispatched {
		t.Fatal("expected the pending task to be dispatched over the channel")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reverse RunCopyTask failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reverse copy never completed")
	}

	// Destination holds exactly the source bytes.
	got, err := os.ReadFile(filepath.Join(fx.diskDir, "dst", "file.bin"))
	if err != nil {
		t.Fatalf("destination must exist after the reverse copy: %v", err)
	}
	if string(got) != "agent task payload" {
		t.Fatalf("reverse-copied content = %q, want %q", got, "agent task payload")
	}

	// The task row reflects the final server-recorded outcome.
	final, err := db.GetAgentTaskByTaskID(task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != model.AgentTaskDone || final.Progress != 100 {
		t.Fatalf("final task state = %+v, want done/100", final)
	}
}
