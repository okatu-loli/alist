package frp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
)

// ServiceID is the extension id of the built-in frp tunnel background service.
const ServiceID = "builtin.frp"

// statusProviderID is the StatusProvider contribution id; it is the {sid} path
// segment of the poll route. The status handler resolves the service by {id}
// alone (design §4), so this only has to be a stable, unique card id.
const statusProviderID = "tunnel"

// Service is the built-in frp tunnel BackgroundService (design §2.2). It manages
// an external frpc process (method B — the frp library is NOT embedded): Run
// renders a frpc.toml, starts `frpc -c <toml>` under the supervised context and
// blocks on it; Status reports a cached snapshot for the frontend status card.
//
// It is a process-wide singleton. All state mutation goes through setState so
// Status (polled concurrently) reads a consistent snapshot, and the token is
// NEVER placed in state/detail/metrics.
//
// Hot-reload (design §2.4): Run owns an inner loop over a resettable sub-context
// (runCtx, derived from the supervised ctx). Each session's frpc is bound to
// runCtx; Reload cancels runCtx to kill the current frpc and re-enter the loop
// with fresh config. runMu guards cancelRun independently of mu so a concurrent
// Reload and a Status poll never contend.
type Service struct {
	mu      sync.Mutex
	state   string
	detail  string
	metrics map[string]any

	runMu     sync.Mutex
	cancelRun context.CancelFunc

	// configLoader, when set, overrides the package-level loadConfig for THIS
	// instance only. It exists so tests can inject a fixed config without a
	// live settings/db layer, without going through a shared mutable package
	// var: a package var written by one test's cleanup and read by a
	// still-running Run goroutine left over from an earlier test (e.g. one
	// whose shutdown took longer than that test's own timeout budget, which
	// -race's instrumentation and a busy full-suite run can both provoke) is a
	// genuine, unsynchronized data race with no happens-before edge between
	// them -- confirmed by `go test -race ./internal/frp/... -count=1` under
	// full-suite load reporting WARNING: DATA RACE at exactly this pair (a
	// runOnce read of the package var vs a t.Cleanup write to it). Giving each
	// Service its own configLoader closure removes the shared mutable state
	// entirely, so no interleaving between any two tests' goroutines can ever
	// race here, regardless of how slowly a goroutine winds down.
	configLoader func() frpConfig
}

// instance is the process-wide singleton registered at boot.
var instance = &Service{state: ext.ServiceStateStopped}

// loadConfig resolves the current config. It is the process-wide default used
// when a Service has no configLoader override (i.e. always, in production --
// only tests set configLoader). See resolveConfig.
var loadConfig = loadFrpConfig

// resolveConfig returns s.configLoader() when set, else falls back to the
// package-level loadConfig. runOnce calls this instead of the package var
// directly; see configLoader's doc comment for why.
func (s *Service) resolveConfig() frpConfig {
	if s.configLoader != nil {
		return s.configLoader()
	}
	return loadConfig()
}

// Manifest identifies the service. Kinds is BackgroundService and its
// Contributions declare a StatusProvider card (design §4): the frontend renders
// a runtime status card on the services page and polls PollPath, which the host
// resolves to this service's Status(). The card carries only public state — the
// token never crosses this boundary.
func (s *Service) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:    ServiceID,
		Name:  map[string]string{"en": "FRP Tunnel", "zh": "FRP 内网穿透"},
		Kinds: []ext.Kind{ext.KindBackgroundService},
		Contributions: ext.Contributions{
			StatusProviders: []ext.StatusProvider{{
				ID:       statusProviderID,
				Title:    map[string]string{"en": "FRP Tunnel", "zh": "FRP 内网穿透"},
				PollPath: "/api/v4/extensions/" + ServiceID + "/status/" + statusProviderID,
			}},
			// Native host settings page (design C6 去重): the /@manage/ prefix marks
			// it a host-rendered page, and the route now points at the generic
			// FeatureConfigPage (/@manage/feature/<id>) instead of the raw FRP
			// SettingList (/@manage/setting/frp) — so the extension manager's link
			// lands on the FRP feature page (general config section + live status
			// card) rather than a duplicate top-level settings group.
			SettingsPages: []ext.SettingsPage{{
				ID:    "frp",
				Title: map[string]string{"en": "FRP", "zh": "内网穿透"},
				Route: "/@manage/feature/" + ServiceID,
			}},
			// The FRP feature declares its own "general" config section (design C6):
			// the seven seeded FRP setting keys, in seed order, rendered by the
			// generic FeatureConfigPage(builtin.frp) alongside the live status card.
			// Parent=self marks this the base feature's general section.
			ConfigSections: []ext.ConfigSection{{
				Parent: ServiceID,
				Title:  map[string]string{"en": "General", "zh": "通用配置"},
				SettingKeys: []string{
					conf.FrpEnabled,
					conf.FrpServerAddr,
					conf.FrpServerPort,
					conf.FrpToken,
					conf.FrpRemotePort,
					conf.FrpProtocol,
					conf.FrpBinaryPath,
				},
				Order: 0,
			}},
		},
	}
}

// Init is a no-op: the service reads live settings inside Run, so there is no
// process-level setup to do here.
func (s *Service) Init(ctx context.Context, host ext.Host) error { return nil }

// Shutdown is a no-op: the running frpc process is bound to Run's context and is
// killed when that context is cancelled.
func (s *Service) Shutdown(ctx context.Context) error { return nil }

func (s *Service) setState(state, detail string, metrics map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.detail = detail
	s.metrics = metrics
}

// Status returns a cheap, non-blocking snapshot. Detail and Metrics never carry
// the token (design §4 token 保密).
func (s *Service) Status(ctx context.Context) (ext.ServiceStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// copy the metrics map so callers can't mutate our snapshot.
	var metrics map[string]any
	if s.metrics != nil {
		metrics = make(map[string]any, len(s.metrics))
		for k, v := range s.metrics {
			metrics[k] = v
		}
	}
	return ext.ServiceStatus{State: s.state, Detail: s.detail, Metrics: metrics}, nil
}

// Run supervises the tunnel for the process lifetime and MUST block until ctx is
// cancelled. It owns an inner loop over a resettable sub-context so a config
// change reconnects immediately (design §2.4): the supervisor above runs Run
// under context.Background(), so hot-reload cannot go through the supervisor — it
// is driven here by Reload cancelling runCtx.
//
// Each iteration derives runCtx from ctx and runs one session (runOnce). On
// return:
//   - ctx cancelled (host shutdown) → return nil, the loop ends.
//   - runCtx cancelled but ctx alive → a Reload killed this session; loop and
//     re-evaluate with fresh config (immediate reconnect).
//   - runOnce returned on its own (frpc exited, or a fatal render/write error)
//     → hand the error to the supervisor so it backs off before restarting.
//     This keeps a crashing frpc on the supervisor's backoff rather than a busy
//     inner reconnect loop.
func (s *Service) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.runMu.Lock()
		s.cancelRun = cancel
		s.runMu.Unlock()

		err := s.runOnce(runCtx)

		s.runMu.Lock()
		s.cancelRun = nil
		s.runMu.Unlock()
		cancel()

		if ctx.Err() != nil {
			return nil
		}
		if runCtx.Err() != nil {
			// Reload cancelled this session while the host is still live: loop and
			// reconnect with the new config.
			continue
		}
		// frpc exited on its own (no reload): let the supervisor back off and
		// restart, avoiding a busy reconnect loop on a config that frps rejects.
		return err
	}
}

// Reload cancels the in-flight session so Run re-enters its loop with fresh
// config (design §2.4 hot-reload). It is a no-op when no session is running
// (cancelRun nil between iterations); the next iteration reads config anyway, so
// no reload is lost. Safe to call concurrently with Run and Status.
func (s *Service) Reload() {
	s.runMu.Lock()
	if s.cancelRun != nil {
		s.cancelRun()
	}
	s.runMu.Unlock()
}

// runOnce performs exactly one session under runCtx and returns when the session
// ends: an idle state (disabled / invalid-config / missing-binary / render or
// write failure) parks on <-runCtx.Done() so a Reload (or ctx cancel) wakes it
// to re-evaluate — it must NOT busy-return, or the outer loop would spin. A
// live tunnel blocks in cmd.Run until frpc exits or runCtx kills it. The token
// is never placed in state/detail/metrics (design §4 token 保密).
func (s *Service) runOnce(runCtx context.Context) error {
	c := s.resolveConfig()

	if !c.Enabled {
		s.setState(ext.ServiceStateStopped, "disabled", map[string]any{"enabled": false})
		<-runCtx.Done()
		return nil
	}
	if err := c.validate(); err != nil {
		s.setState(ext.ServiceStateError, err.Error(), map[string]any{"enabled": true})
		<-runCtx.Done()
		return nil
	}
	if _, err := exec.LookPath(c.BinaryPath); err != nil {
		s.setState(ext.ServiceStateError, "frpc not found: "+c.BinaryPath, map[string]any{"enabled": true})
		<-runCtx.Done()
		return nil
	}

	tomlBytes, err := renderFrpcToml(c)
	if err != nil {
		s.setState(ext.ServiceStateError, "failed to render frpc config", map[string]any{"enabled": true})
		<-runCtx.Done()
		return nil
	}
	dir := filepath.Join(flags.DataDir, "frp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.setState(ext.ServiceStateError, "failed to create frp data dir", map[string]any{"enabled": true})
		<-runCtx.Done()
		return nil
	}
	tomlPath := filepath.Join(dir, "frpc.toml")
	// 0600: the file holds the frps token (design §4 frpc.toml 0600 权限).
	if err := os.WriteFile(tomlPath, tomlBytes, 0600); err != nil {
		s.setState(ext.ServiceStateError, "failed to write frpc config", map[string]any{"enabled": true})
		<-runCtx.Done()
		return nil
	}
	// os.WriteFile only applies 0600 when it CREATES the file; a pre-existing
	// frpc.toml (a prior run, or one an attacker pre-created world-readable to
	// harvest the token) keeps its old, possibly wider, permissions. Chmod
	// unconditionally so the token file is never group/other-readable (design §4).
	if err := os.Chmod(tomlPath, 0600); err != nil {
		s.setState(ext.ServiceStateError, "failed to secure frpc config", map[string]any{"enabled": true})
		<-runCtx.Done()
		return nil
	}

	// Fixed argv: `frpc -c <path>`. User input is never spliced into the command
	// line — the token travels through the 0600 toml file, never argv (design §4).
	cmd := exec.CommandContext(runCtx, c.BinaryPath, "-c", tomlPath)
	// Public address is safe to surface; the token is not part of it.
	addr := c.ServerAddr + ":" + strconv.Itoa(c.RemotePort)
	s.setState(ext.ServiceStateRunning, addr, map[string]any{"enabled": true})

	err = cmd.Run()
	if runCtx.Err() != nil {
		// Cancelled (host shutdown / hot-reload): CommandContext killed frpc.
		s.setState(ext.ServiceStateStopped, "stopped", map[string]any{"enabled": true})
		return nil
	}
	// frpc exited on its own: report error and let the supervisor back off and
	// restart (reconnect).
	s.setState(ext.ServiceStateError, "frpc exited", map[string]any{"enabled": true})
	return err
}
