// Package cli builds the `agent` cobra command tree shared by both entry
// points that expose the remote AList agent (design W5):
//
//   - the main `alist` binary, which mounts NewAgentCmd(...) as a subcommand
//     (see cmd/agent.go in the core repo — a ~15-line shell around this
//     package) so `alist agent join/run/serve` keeps working unchanged
//     (design R11: dual entry points, not a breaking move);
//   - the standalone `alist-agent` binary (cmd/alist-agent/main.go in this
//     module), which mounts the very same command tree as its root.
//
// All three subcommands (join/run/serve) and their flags/help text/log
// wording are unchanged from the core repo's former cmd/agent.go — this is a
// verbatim behavioral port. The two things that file pulled from the core
// module (internal/conf.Version and pkg/utils.Log) are now supplied by the
// caller instead: a BuildInfo{Version} value and a *logrus.Logger, so this
// package has zero AList core imports (BOUNDARY RULE, ../doc.go).
package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/AlistGo/alist-agent/agent"
	"github.com/AlistGo/alist-agent/channel"
	"github.com/AlistGo/alist-agent/proto"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// BuildInfo carries the version string the core repo's cmd/agent.go used to
// read straight off internal/conf.Version. Injected instead so this module
// never imports the core.
type BuildInfo struct {
	Version string
}

// defaultAgentDataDir is agent join/run/serve's default --data value —
// deliberately distinct from the alist server's own --data default ("data").
const defaultAgentDataDir = "agent_data"

// agentChannelPath is the single route the reverse-connection listener exposes
// (design R2). The server dials ws://<listen-addr><agentChannelPath>.
const agentChannelPath = "/agentchannel"

// agentPairPath is the FIRST-CONTACT token pairing route the acceptor exposes
// ONLY under `serve --pair` (design F). The server dials ws://<listen><agentPairPath>
// once to pair; the acceptor stays DISARMED until the operator pastes a one-time
// token into the pairing web (--pair-web) and disarms again after the first
// success or on token TTL expiry.
const agentPairPath = "/agentpair"

// agentHeartbeatInterval is how often `agent run` sends a heartbeat.
const agentHeartbeatInterval = 30 * time.Second

// agentTaskTimeout bounds one delegated copy task's whole execution
// (download + upload) once a heartbeat hands one back. Generous — a
// cross-drive copy can legitimately take a while — but not unbounded, so a
// wedged download/upload can never hang forever. It bounds ONLY the task's
// own background goroutine (see agent.Runner.TaskTimeout) — it is entirely
// independent of, and never blocks, the heartbeat loop itself (design
// W12-4 (A): concurrent heartbeat — see internal/agent/runner.go's
// top-of-file doc for why that decoupling is safety-critical, not just a
// responsiveness nicety).
const agentTaskTimeout = time.Hour

// heartbeatCallTimeout bounds a SINGLE heartbeat HTTP call — independent of
// agentTaskTimeout, which bounds a dispatched task's own background
// goroutine. A slow/wedged heartbeat call must not itself hang past this,
// but it also must never be extended or blocked by however long the
// current task has left to run (see agent.Runner).
const heartbeatCallTimeout = 15 * time.Second

// reverseRepairGuard decides whether `serve --pair` may arm the pairing
// acceptor. It REFUSES (returns an error) when the agent is ALREADY paired (a
// non-empty pinned ServerPubKey) and --repair was not given, so a persistent
// `serve --pair` service cannot silently re-pair on every restart and let an
// attacker overwrite an already-good pinned key. With --repair (or when the
// agent is not yet paired) it allows arming; the SAS confirm gate still applies
// on top of that.
func reverseRepairGuard(cfg *agent.ClientConfig, repair bool) error {
	if cfg != nil && cfg.ServerPubKey != "" && !repair {
		return errors.New("本 Agent 已配对;如需重新配对请加 --repair(会覆盖已钉服务器密钥)")
	}
	return nil
}

// NewAgentCmd builds the `agent` command tree (join/run/serve), identical in
// behavior to the core repo's former cmd/agent.go. info supplies the version
// string reported to the server on `agent join` (formerly internal/conf.Version);
// log is where every line this command tree used to send to utils.Log now
// goes instead.
func NewAgentCmd(info BuildInfo, log *logrus.Logger) *cobra.Command {
	var (
		agentJoinHost                string
		agentJoinCode                string
		agentJoinName                string
		agentDataDir                 string
		agentServeListen             string
		agentServePair               bool
		agentServeRepair             bool
		agentServePairWeb            string
		agentServePairWebAllowPublic bool
	)

	// agentServeLogRing is the process-wide log ring the reverse `serve` command
	// attaches to log at startup (design SP-G1). Every ReverseAgent built in
	// buildReverseChannelListener's OnChannel below hangs its LogRing off it, so the
	// server can tail the agent's recent logs over the authenticated channel. nil
	// outside `serve` (outbound `agent run` has no server-initiated channel to be
	// tailed over), in which case the log.tail method answers with an empty tail.
	var agentServeLogRing *agent.LogRing

	// agentServeTaskGate is the process-wide one-task-at-a-time guard (channel-
	// stability wave 1, design SP-agent-channel-stability). It is shared across
	// EVERY ReverseAgent this `agent serve` process constructs — including
	// across a re-pair that swaps in a brand new channel.Listener/Channel — so
	// that even a brief window with two channels alive at once (a redial racing
	// the old channel's teardown; the listener's own single-live-channel swap
	// closes the old one, but that close is not instantaneous with respect to a
	// dispatch already read off its wire) can never run the SAME server-pushed
	// task twice. Built once per process, same lifetime as agentServeLogRing.
	agentServeTaskGate := agent.NewTaskGate()

	// buildReverseChannelListener builds the R2 pinned-key data listener from a
	// paired config + identity. Extracted so both the already-paired serve path and
	// the post-pairing go-live path (serve --pair) construct an identically-wired
	// listener. It returns an error if cfg has no valid pinned server key (fail
	// closed — NewListener enforces this too).
	buildReverseChannelListener := func(cfg *agent.ClientConfig, identity *agent.Identity) (*channel.Listener, error) {
		if cfg.ServerPubKey == "" {
			return nil, errors.New("agent serve: no pinned server public key")
		}
		serverPub, err := base64.StdEncoding.DecodeString(cfg.ServerPubKey)
		if err != nil || len(serverPub) != ed25519.PublicKeySize {
			return nil, errors.New("agent serve: pinned server public key is malformed")
		}
		return channel.NewListener(channel.ListenerConfig{
			AgentID:         cfg.AgentID,
			AgentPriv:       identity.PrivKey,
			PinnedServerPub: ed25519.PublicKey(serverPub),
			OnChannel: func(ch *channel.Channel) {
				// R4 run-loop: the channel is authenticated and live. Bind a
				// ReverseAgent to it (R3) so a server-pushed task.dispatch runs
				// the copy through RunCopyTaskWith over the channelCapClient —
				// every cap call still independently re-authorized server-side.
				// One ReverseAgent per server connection; it and its handler die
				// with the channel. NewReverseAgent registers the dispatch
				// handler on ch.
				log.Infof("agent serve: reverse channel established")
				ra := agent.NewReverseAgent(ch, agentServeTaskGate)
				ra.TaskTimeout = agentTaskTimeout
				// Expose this agent's recent logs to the server over agent.log.tail
				// (design SP-G1). The ring is process-wide (attached to log at
				// serve startup), so every channel's ReverseAgent tails the same
				// captured history.
				ra.LogRing = agentServeLogRing
				ra.OnTaskStart = func(taskID string) { log.Infof("agent serve: dispatched task %s", taskID) }
				ra.OnTaskSkipped = func(newTaskID, activeTaskID string) {
					log.Warnf("agent serve: got task %s while already running %s; ignoring (one task at a time)", newTaskID, activeTaskID)
				}
				ra.OnTaskDone = func(taskID string, err error) {
					if err != nil {
						log.Errorf("agent serve: task %s failed: %v", taskID, err)
						return
					}
					log.Infof("agent serve: task %s done", taskID)
				}
				go func() {
					<-ch.Done()
					log.Warnf("agent serve: reverse channel closed: %v", ch.Err())
				}()
			},
		})
	}

	// AgentCmd groups the remote-agent CLI (design W12-2a): a companion process
	// that pairs to a running alist server and, once paired, proves its
	// identity on every heartbeat via a locally-generated ed25519 keypair. It
	// deliberately never calls Init()/bootstrap — an agent is a lightweight
	// network client, not a second alist server, and its own --data directory
	// (default "agent_data", intentionally distinct from `alist server`'s
	// default "data") is the ONLY local state it touches: an identity keypair
	// and which server/agent_id it is paired to. All actual logic (identity
	// load/create, signing, the two HTTP calls) lives in internal/agent so it
	// is unit-testable without spawning this CLI at all — see
	// internal/agent/{identity,config,client}.go and their tests.
	agentCmd := &cobra.Command{
		Use:   "agent",
		Short: "Run or pair a remote alist agent",
		Long: `Pair a companion machine to an alist server and run it as a remote agent.

An agent generates and keeps its own ed25519 identity under --data
(agent_identity.json, 0600) and authenticates every heartbeat to the server
by signing it with that key — see 'alist agent join' and 'alist agent run'.`,
	}

	// agentJoinCmd completes pairing: generate-or-reuse an identity, POST it
	// plus the operator-supplied pairing code to the server, and persist the
	// server-assigned AgentID for a later `agent run`.
	agentJoinCmd := &cobra.Command{
		Use:   "join",
		Short: "Pair this machine to an alist server using an admin-issued pairing code",
		Run: func(cmd *cobra.Command, args []string) {
			if agentJoinHost == "" || agentJoinCode == "" {
				log.Error("agent join: --host and --code are both required")
				os.Exit(1)
			}

			identity, err := agent.LoadOrCreateIdentity(agentDataDir)
			if err != nil {
				log.Errorf("agent join: failed to load/create identity: %+v", err)
				os.Exit(1)
			}

			name := agentJoinName
			if name == "" {
				if h, herr := os.Hostname(); herr == nil && h != "" {
					name = h
				} else {
					name = "agent"
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := agent.PairComplete(ctx, agentJoinHost, agent.PairJoinParams{
				Code:     agentJoinCode,
				Identity: identity,
				Name:     name,
				Platform: runtime.GOOS + "/" + runtime.GOARCH,
				Version:  info.Version,
			})
			if err != nil {
				log.Errorf("agent join: pairing failed: %v", err)
				os.Exit(1)
			}

			if err := agent.SaveClientConfig(agentDataDir, agent.ClientConfig{
				Host:    agentJoinHost,
				AgentID: resp.AgentID,
				// design R1: pin the server's delivered public key for a later
				// reverse-connection wave to verify against. Empty if the server
				// wired no reverse identity; nothing verifies against it yet.
				ServerPubKey: resp.ServerPubKey,
			}); err != nil {
				log.Errorf("agent join: paired (agent_id=%s) but failed to persist local config: %+v", resp.AgentID, err)
				os.Exit(1)
			}

			// design W12-2b: derive the SAS ENTIRELY LOCALLY — from the pairing
			// code the operator just typed plus this identity's OWN generated
			// public key — rather than displaying resp.SAS as-is. This is not
			// interchangeable with printing resp.SAS: an on-path attacker able to
			// substitute the pubkey in the pair/complete request controls that
			// response too and could relay it back unmodified, which would make
			// a tampered pairing look self-consistent. Deriving locally means
			// this printed sequence reflects ONLY what this machine actually
			// generated, so it can only match the server's independently-derived
			// copy (visible on the admin's confirm page) if the pubkey the
			// server received really is this one. See internal/agent/sas.go's
			// top-of-file doc for the full reasoning.
			localSAS := proto.DeriveSAS(agentJoinCode, identity.PubKey)
			if len(resp.SAS) > 0 && strings.Join(resp.SAS, " ") != strings.Join(localSAS, " ") {
				// The server's own derivation (from what it actually received)
				// already disagrees with what this machine computed — that is
				// itself live evidence of tampering in transit, independent of
				// whatever the admin's page ends up showing. Surface it loudly;
				// do not silently prefer either value.
				log.Warnf("agent join: WARNING — the server's returned SAS does not match what this machine independently computed. This can indicate the pairing request was tampered with in transit. Do NOT confirm this pairing without investigating.")
			}

			fmt.Printf("Paired successfully. agent_id=%s\n", resp.AgentID)
			fmt.Println()
			fmt.Println("=== SAS (Short Authentication String) ===")
			fmt.Printf("  %s\n", strings.Join(localSAS, "  "))
			fmt.Println("On the main server's page, compare this against the emoji shown for this agent.")
			fmt.Println("They must match EXACTLY. If they don't, do NOT confirm — this pairing may have been intercepted.")
			fmt.Println("===========================================")
			fmt.Println()
			fmt.Println("This agent stays INACTIVE (heartbeats will be refused) until an admin visually matches the SAS above and confirms it on the server.")
			fmt.Println("It also has NO storage access yet even once confirmed — an admin must separately grant it a storage whitelist.")
			fmt.Println("Run `alist agent run` to start sending heartbeats (it will wait for the admin's confirmation).")
		},
	}

	// agentRunCmd loads a previously joined agent's identity+config and sends
	// an authenticated heartbeat on a fixed interval until interrupted. Design
	// W12-4 (A): the heartbeat loop is built on agent.Runner, which ticks on
	// its OWN schedule (agentHeartbeatInterval) independently of however long a
	// dispatched task takes — a task runs in its own background goroutine, so
	// a multi-hour copy can never starve the heartbeat that keeps this agent's
	// LastSeen fresh (and therefore never mistaken for dead by the server's
	// db.RequeueStaleAgentTasks sweep; see that function's doc for the paired
	// safety property). Renewed capability tokens (a fresh one is minted on
	// every heartbeat while a task is active — see
	// server/handles.dispatchAgentTask) are threaded into the in-flight copy
	// via agent.TaskToken, so a copy that outlives one token's TTL keeps
	// working. This function itself is thin CLI glue: load identity/config,
	// build the Runner wired to the real Heartbeat/RunCopyTask calls, run it
	// until an OS signal. All the actual behavior lives in internal/agent (see
	// runner.go, task.go) so it is unit-testable without spawning this CLI at
	// all — see internal/agent/runner_test.go.
	agentRunCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the agent, sending authenticated heartbeats to its paired server and executing any delegated task",
		Run: func(cmd *cobra.Command, args []string) {
			identity, err := agent.LoadOrCreateIdentity(agentDataDir)
			if err != nil {
				log.Fatalf("agent run: failed to load identity: %+v", err)
			}
			cfg, err := agent.LoadClientConfig(agentDataDir)
			if err != nil {
				log.Fatalf("agent run: not paired yet (run `alist agent join` first): %+v", err)
			}
			// Outbound mode is the one that dials the server, so Host is required
			// HERE (a reverse token pairing legitimately stores no host — that
			// config belongs to `agent serve`, not `agent run`).
			if cfg.Host == "" {
				log.Fatal("agent run: config has no server host — this agent was paired in REVERSE mode (`agent serve`), or needs `alist agent join` for outbound mode")
			}

			log.Infof("agent starting: agent_id=%s server=%s", cfg.AgentID, cfg.Host)

			runner := agent.NewRunner(
				func(ctx context.Context) (*agent.HeartbeatResponse, error) {
					ctx, cancel := context.WithTimeout(ctx, heartbeatCallTimeout)
					defer cancel()
					return agent.Heartbeat(ctx, cfg.Host, identity, cfg.AgentID)
				},
				func(ctx context.Context, task agent.Task, tokenFn agent.TokenFunc) error {
					return agent.RunCopyTask(ctx, cfg.Host, task, tokenFn)
				},
			)
			runner.TaskTimeout = agentTaskTimeout
			runner.OnHeartbeatOK = func() { log.Debugf("agent run: heartbeat ok") }
			runner.OnHeartbeatError = func(err error) {
				// design W12-2b: a freshly (re-)paired agent's heartbeat is
				// refused (403) until an admin SAS-confirms it — that is an
				// EXPECTED, waiting-for-a-human state, not a scary failure,
				// so it gets its own calmer message instead of the generic
				// "heartbeat failed" one. Runner.Run keeps ticking on its
				// normal interval regardless, so this naturally keeps
				// retrying until the admin confirms.
				var rej *agent.RejectedError
				if errors.As(err, &rej) && rej.Code == 403 {
					log.Warnf("agent run: waiting for admin confirmation on the server (heartbeat refused: %s) — will keep retrying", rej.Message)
					return
				}
				log.Warnf("agent run: heartbeat failed: %v", err)
			}
			runner.OnDisabled = func() { log.Warnf("agent run: heartbeat ok but this agent is disabled by the admin") }
			runner.OnTaskStart = func(taskID string) { log.Infof("agent run: dispatched task %s", taskID) }
			runner.OnTaskRenew = func(taskID string) {
				log.Debugf("agent run: renewed capability token for in-flight task %s", taskID)
			}
			runner.OnTaskSkipped = func(newTaskID, activeTaskID string) {
				log.Warnf("agent run: got task %s while already running %s; ignoring (one task at a time)", newTaskID, activeTaskID)
			}
			runner.OnTaskDone = func(taskID string, err error) {
				if err != nil {
					log.Errorf("agent run: task %s failed: %v", taskID, err)
					return
				}
				log.Infof("agent run: task %s done", taskID)
			}

			ctx, cancel := context.WithCancel(context.Background())
			quit := make(chan os.Signal, 1)
			signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-quit
				log.Info("agent run: shutting down")
				cancel()
			}()
			runner.Run(ctx, agentHeartbeatInterval)
		},
	}

	// agentServeCmd runs the agent in REVERSE mode (design R2): instead of dialing
	// out to the server, it LISTENS on --listen for the server to dial IN over a
	// persistent WebSocket channel. It refuses to start unless a server public key
	// was pinned at pairing (agent_config.json's server_pubkey) — reverse mode's
	// whole security rests on the agent verifying the server against that pinned
	// key, so with no key there is nothing to trust and the surface must not open
	// (fail closed). R2 only holds the authenticated channel alive + logs; R3
	// registers the task/cap handlers on it.
	agentServeCmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the agent in reverse mode: listen for the paired server to dial in over a persistent channel",
		Run: func(cmd *cobra.Command, args []string) {
			if agentServeListen == "" {
				log.Error("agent serve: --listen ADDR is required")
				os.Exit(1)
			}
			// Capture the agent's own logs into a bounded ring so the paired server
			// can tail them over the authenticated channel (design SP-G1). Attached
			// BEFORE any serve log line so early startup is captured too. SECURITY:
			// these lines egress to the server — see internal/agent/logring.go; the
			// agent must never log secrets.
			agentServeLogRing = agent.AttachLogRing(log, agent.DefaultLogRingCapacity)
			identity, err := agent.LoadOrCreateIdentity(agentDataDir)
			if err != nil {
				log.Fatalf("agent serve: failed to load identity: %+v", err)
			}

			mux := http.NewServeMux()

			// dataListener holds the CURRENT R2 pinned-key data listener. It is nil
			// until the agent is paired; the /agentchannel handler serves 503 while
			// nil. Under --pair a successful first-contact pairing swaps a freshly
			// built listener in atomically, so /agentchannel goes live WITHOUT a
			// restart — the server can then dial the data channel once an admin
			// confirms. Without --pair the listener is built up front (the agent
			// must already be paired).
			var dataListener atomic.Pointer[channel.Listener]
			mux.Handle(agentChannelPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				l := dataListener.Load()
				if l == nil {
					http.Error(w, "agent not paired yet", http.StatusServiceUnavailable)
					return
				}
				l.ServeHTTP(w, r)
			}))

			if agentServePair {
				// --- opt-in FIRST-CONTACT pairing mode (design E2) -------------------
				// The agent may run even when NOT yet paired (no pinned server key).
				// It mounts the bounded, single-success pairing acceptor alongside the
				// data route. Load any existing config best-effort (a never-joined
				// agent has none) — only Host is preserved from it.
				base := agent.ClientConfig{}
				var pairedCfg *agent.ClientConfig
				if cfg, cerr := agent.LoadClientConfig(agentDataDir); cerr == nil {
					base = *cfg
					pairedCfg = cfg
				} else if !errors.Is(cerr, os.ErrNotExist) {
					// A config that EXISTS but fails to load must be loud: silently
					// treating a paired agent as never-paired both leaves the data
					// listener down (503 forever) and bypasses the --repair guard.
					log.Fatalf("agent serve --pair: existing config failed to load (fix or remove %s/agent_config.json): %+v", agentDataDir, cerr)
				}

				// 🔒 --repair guard: refuse to re-arm the pairing acceptor over an
				// already-pinned server key unless the operator explicitly asks. This
				// stops a persistent `serve --pair` service from silently re-pairing on
				// every restart and letting an attacker overwrite a good pinned key.
				if err := reverseRepairGuard(pairedCfg, agentServeRepair); err != nil {
					log.Fatalf("agent serve --pair: %v", err)
				}

				// Already paired (and, if so, --repair was given)? Bring the data
				// channel up immediately with the existing, previously-confirmed key —
				// re-pairing still goes through the confirm gate below before any NEW
				// key is pinned.
				if pairedCfg != nil {
					if l, lerr := buildReverseChannelListener(pairedCfg, identity); lerr == nil {
						dataListener.Store(l)
					} else {
						// e.g. an outbound-paired (join) config with no pinned server
						// key: not fatal — a fresh token pairing below can still pin
						// one — but never silent, /agentchannel serves 503 until then.
						log.Warnf("agent serve --pair: paired config present but data listener not built (%v); /agentchannel stays 503 until a new pairing pins a server key", lerr)
					}
				}

				acceptor := agent.NewPairingAcceptor(identity, agentDataDir, base)

				// --pair-web is REQUIRED: the operator supplies the one-time token by
				// PASTING it into the pairing web (that paste+arm IS the affirmative
				// action). The terminal y/n confirm flow was removed (design F, F2), so
				// without the web there is no token source and no consent surface.
				if agentServePairWeb == "" {
					log.Fatal("agent serve --pair: --pair-web ADDR is required — paste the server's one-time token there to arm pairing (the terminal confirm flow was removed)")
				}

				// 🔒 Confidentiality of the token bind (design F Critical #3): default
				// 127.0.0.1 (reach it over an SSH tunnel); a NON-loopback plain-HTTP bind
				// is REFUSED unless --pair-web-allow-public is passed, and even then it
				// warns that T transits in cleartext without TLS.
				webAddr, webWarn, werr := agent.ResolvePairWebBind(agentServePairWeb, agentServePairWebAllowPublic)
				if werr != nil {
					log.Fatalf("agent serve --pair-web: %v", werr)
				}
				if webWarn != "" {
					log.Warn(webWarn)
				}

				// The pairing web owns the token lifecycle (paste T + arm; TTL/single-use/
				// auto-disarm) and wires the acceptor's consent gate to auto-true — a
				// passing token verify pins + goes live with no separate confirm.
				// NewPairingWeb starts the acceptor DISARMED; it arms on a pasted token.
				pweb := agent.NewPairingWeb(acceptor)
				pweb.OnPaired = func(cfg agent.ClientConfig, serverPub []byte, fingerprint string) {
					// Verified + pinned: bring the data channel live with the fresh key.
					if l, lerr := buildReverseChannelListener(&cfg, identity); lerr == nil {
						dataListener.Store(l)
					} else {
						log.Errorf("agent serve: pinned server key but failed to build data listener: %v", lerr)
					}
					log.Infof("agent serve: 令牌校验通过,已钉服务器密钥并激活数据通道 (agent_id=%s, 服务器密钥指纹=%s);服务器侧仍需管理员确认后方可下发任务", cfg.AgentID, fingerprint)
				}
				mux.Handle(agentPairPath, acceptor)

				// The LOCAL pairing-web control server, SEPARATE from the public --listen
				// acceptor: the operator pastes T here to arm the acceptor.
				webSrv := &http.Server{Addr: webAddr, Handler: pweb, ReadHeaderTimeout: 15 * time.Second}
				go func() {
					log.Infof("agent serve: 配对网页在 http://%s (在此粘贴服务器一次性令牌以布防配对)", webAddr)
					if err := webSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						log.Fatalf("agent serve: pairing web failed: %v", err)
					}
				}()
				defer func() {
					shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer shutdownCancel()
					_ = webSrv.Shutdown(shutdownCtx)
				}()

				// 🔒 The acceptor starts DISARMED and only opens on a pasted token; it
				// disarms after ONE successful pairing or when the token TTL expires.
				log.Warnf("agent serve: reverse pairing acceptor mounted at ws://%s%s — DISARMED until you paste a one-time token into the pairing web; it disarms again after ONE successful pairing or on token TTL expiry", agentServeListen, agentPairPath)
				defer acceptor.Disarm()
			} else {
				// --- default mode (design R2): must already be paired ----------------
				cfg, err := agent.LoadClientConfig(agentDataDir)
				if err != nil {
					log.Fatalf("agent serve: not paired yet (run `alist agent join`, or `alist agent serve --pair` to accept a reverse first-contact pairing): %+v", err)
				}
				if cfg.ServerPubKey == "" {
					log.Fatal("agent serve: refusing to start — this agent has no pinned server public key (server_pubkey). Reverse mode requires the key delivered at pairing; re-pair (server dials `serve --pair`), run `alist agent join` for outbound mode, or start with --pair to accept a reverse pairing.")
				}
				listener, err := buildReverseChannelListener(cfg, identity)
				if err != nil {
					log.Fatalf("agent serve: %v", err)
				}
				dataListener.Store(listener)
			}

			srv := &http.Server{
				Addr:              agentServeListen,
				Handler:           mux,
				ReadHeaderTimeout: 15 * time.Second,
			}

			ctx, cancel := context.WithCancel(context.Background())
			quit := make(chan os.Signal, 1)
			signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-quit
				log.Info("agent serve: shutting down")
				cancel()
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shutdownCancel()
				_ = srv.Shutdown(shutdownCtx)
			}()

			log.Infof("agent serve: reverse data listener on %s%s, waiting for the paired server to dial in", agentServeListen, agentChannelPath)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("agent serve: listener failed: %v", err)
			}
			<-ctx.Done()
		},
	}

	agentCmd.AddCommand(agentJoinCmd)
	agentCmd.AddCommand(agentRunCmd)
	agentCmd.AddCommand(agentServeCmd)

	agentJoinCmd.Flags().StringVar(&agentJoinHost, "host", "", "alist server base URL, e.g. http://localhost:5244 (required)")
	agentJoinCmd.Flags().StringVar(&agentJoinCode, "code", "", "pairing code issued by an admin (required)")
	agentJoinCmd.Flags().StringVar(&agentJoinName, "name", "", "display name for this agent (defaults to the machine's hostname)")
	agentJoinCmd.Flags().StringVar(&agentDataDir, "data", defaultAgentDataDir, "agent data folder (identity + pairing config); distinct from the server's --data")

	agentRunCmd.Flags().StringVar(&agentDataDir, "data", defaultAgentDataDir, "agent data folder (identity + pairing config); distinct from the server's --data")

	agentServeCmd.Flags().StringVar(&agentServeListen, "listen", "", "address to listen on for reverse server connections, e.g. 0.0.0.0:5343 (required)")
	agentServeCmd.Flags().StringVar(&agentDataDir, "data", defaultAgentDataDir, "agent data folder (identity + pairing config); distinct from the server's --data")
	agentServeCmd.Flags().BoolVar(&agentServePair, "pair", false, "opt-in: also accept a one-time TOKEN-authenticated reverse FIRST-CONTACT pairing (a server with no public IP dials in to pair). Bounded, single-success acceptor that stays DISARMED until you paste the server's one-time token into the pairing web (--pair-web, required with --pair) and disarms again after one successful pairing or on token TTL expiry; allows serving while not yet paired")
	agentServeCmd.Flags().BoolVar(&agentServeRepair, "repair", false, "with --pair: allow RE-pairing an already-paired agent (overwrites the pinned server key once a new token pairing verifies). Without it, `serve --pair` refuses to re-arm on an already-paired agent")
	agentServeCmd.Flags().StringVar(&agentServePairWeb, "pair-web", "", "with --pair: address (or bare port) for the LOCAL pairing web where you paste the server's one-time token. Defaults the host to 127.0.0.1 (reach it via `ssh -L`); a non-loopback plain-HTTP bind is refused unless --pair-web-allow-public")
	agentServeCmd.Flags().BoolVar(&agentServePairWebAllowPublic, "pair-web-allow-public", false, "acknowledge binding --pair-web to a non-loopback address over PLAIN HTTP (the pasted token transits in CLEARTEXT unless you front it with TLS). Without this, a non-loopback bind is refused")

	return agentCmd
}
