package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	ext "github.com/AlistGo/alist-ext"
	toml "github.com/pelletier/go-toml/v2"
)

// pluginID is the extension id of the FRP tunnel plugin — it MUST match
// plugin.json's "id" and is the {id} path segment of the status poll route.
const pluginID = "cn.alist.frp"

// statusProviderID is the StatusProvider contribution id; it is the {sid} path
// segment of the poll route. The status handler resolves the service by {id}
// alone (design §4), so this only has to be a stable, unique card id.
const statusProviderID = "tunnel"

// frpService is the out-of-tree FRP tunnel BackgroundService. It manages an
// external frpc process (method B — the frp library is NOT embedded): Run
// renders an frpc.toml, starts `frpc -c <toml>` under the supervised context
// and blocks on it; Status reports a cached snapshot for the frontend status
// card.
//
// All state mutation goes through setState so Status (polled concurrently)
// reads a consistent snapshot, and the token is NEVER placed in
// state/detail/metrics (design §4 token 保密).
//
// This mirrors internal/frp.Service, but with two differences a plugin process
// forces: (1) config comes from env, not settings; (2) there is no inner
// hot-reload loop — the host's Start/Stop drives the lifecycle by starting Run
// under a fresh context and cancelling it, so Run is a single supervised pass.
type frpService struct {
	mu      sync.Mutex
	state   string
	detail  string
	metrics map[string]any
}

// newFrpService constructs the service in the stopped state.
func newFrpService() *frpService {
	return &frpService{state: ext.ServiceStateStopped}
}

// loadConfig resolves the current config. It is a package var so tests can
// inject a fixed config without touching the process environment.
var loadConfig = loadEnvConfig

// Manifest identifies the service. It MUST be JSON-equivalent to plugin.json
// (verified by TestManifestMatchesPluginJSON). Kinds is BackgroundService and
// its Contributions declare a StatusProvider card (design §4): the frontend
// renders a runtime status card and polls PollPath, which the host resolves to
// this service's Status(). The card carries only public state — the token never
// crosses this boundary.
func (s *frpService) Manifest() ext.Manifest {
	return ext.Manifest{
		ID:      pluginID,
		Name:    map[string]string{"en": "FRP Tunnel", "zh": "FRP 内网穿透"},
		Version: "0.1.0",
		CoreAPI: "^0.1.0",
		Kinds:   []ext.Kind{ext.KindBackgroundService},
		Provenance: ext.Provenance{
			Source:    "official",
			License:   "AGPL-3.0",
			SourceURL: "https://github.com/alist-org/alist",
			Pricing:   "free",
		},
		Permissions: ext.Permissions{
			Network:    []string{"*"},
			Filesystem: "temp",
		},
		Contributions: ext.Contributions{
			StatusProviders: []ext.StatusProvider{{
				ID:       statusProviderID,
				Title:    map[string]string{"en": "FRP Tunnel", "zh": "FRP 内网穿透"},
				PollPath: "/api/v4/extensions/" + pluginID + "/status/" + statusProviderID,
			}},
		},
	}
}

// Init is a no-op: the service reads its config from env inside Run, so there is
// no process-level setup to do here.
func (s *frpService) Init(ctx context.Context, host ext.Host) error { return nil }

// Shutdown is a no-op: the running frpc process is bound to Run's context and is
// killed when that context is cancelled.
func (s *frpService) Shutdown(ctx context.Context) error { return nil }

func (s *frpService) setState(state, detail string, metrics map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.detail = detail
	s.metrics = metrics
}

// Status returns a cheap, non-blocking snapshot. Detail and Metrics never carry
// the token (design §4 token 保密).
func (s *frpService) Status(ctx context.Context) (ext.ServiceStatus, error) {
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

// Run supervises the tunnel and MUST block until ctx is cancelled (host
// shutdown or a Stop). It performs one session: an idle state (invalid config /
// missing binary / render or write failure) parks on <-ctx.Done() so it never
// busy-returns; a live tunnel blocks in cmd.Run until frpc exits or ctx kills
// it. The token is never placed in state/detail/metrics (design §4). On an
// unexpected frpc exit it returns the error so the host supervisor backs off
// and restarts.
func (s *frpService) Run(ctx context.Context) error {
	c := loadConfig()

	if err := c.validate(); err != nil {
		s.setState(ext.ServiceStateError, err.Error(), nil)
		<-ctx.Done()
		return nil
	}
	if _, err := exec.LookPath(c.BinaryPath); err != nil {
		s.setState(ext.ServiceStateError, "frpc not found: "+c.BinaryPath, nil)
		<-ctx.Done()
		return nil
	}

	tomlBytes, err := renderFrpcToml(c)
	if err != nil {
		s.setState(ext.ServiceStateError, "failed to render frpc config", nil)
		<-ctx.Done()
		return nil
	}
	dir := filepath.Join(os.TempDir(), "alist-frp-plugin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.setState(ext.ServiceStateError, "failed to create frp data dir", nil)
		<-ctx.Done()
		return nil
	}
	tomlPath := filepath.Join(dir, "frpc.toml")
	// 0600: the file holds the frps token (design §4 frpc.toml 0600 权限).
	if err := os.WriteFile(tomlPath, tomlBytes, 0600); err != nil {
		s.setState(ext.ServiceStateError, "failed to write frpc config", nil)
		<-ctx.Done()
		return nil
	}
	// os.WriteFile only applies 0600 when it CREATES the file; a pre-existing
	// frpc.toml (a prior run, or one an attacker pre-created world-readable to
	// harvest the token) keeps its old, possibly wider, permissions. Chmod
	// unconditionally so the token file is never group/other-readable (design §4).
	if err := os.Chmod(tomlPath, 0600); err != nil {
		s.setState(ext.ServiceStateError, "failed to secure frpc config", nil)
		<-ctx.Done()
		return nil
	}

	// Fixed argv: `frpc -c <path>`. User input is never spliced into the command
	// line — the token travels through the 0600 toml file, never argv (design §4).
	cmd := exec.CommandContext(ctx, c.BinaryPath, "-c", tomlPath)
	// Public address is safe to surface; the token is not part of it.
	addr := c.ServerAddr + ":" + strconv.Itoa(c.RemotePort)
	s.setState(ext.ServiceStateRunning, addr, map[string]any{"remote_port": c.RemotePort})

	err = cmd.Run()
	if ctx.Err() != nil {
		// Cancelled (host shutdown / Stop): CommandContext killed frpc.
		s.setState(ext.ServiceStateStopped, "stopped", nil)
		return nil
	}
	// frpc exited on its own: report error and let the host supervisor back off
	// and restart (reconnect).
	s.setState(ext.ServiceStateError, "frpc exited", nil)
	return err
}

// frpConfig is the resolved tunnel configuration read from the plugin process
// environment.
type frpConfig struct {
	ServerAddr string
	ServerPort int
	Token      string
	RemotePort int
	Protocol   string
	BinaryPath string
	LocalPort  int
}

// loadEnvConfig reads the tunnel config from the environment the host injects.
// The plugin process has no settings/db layer, so every knob is an env var with
// a sensible default (port 7000, protocol tcp, binary "frpc", local port 5244).
func loadEnvConfig() frpConfig {
	return frpConfig{
		ServerAddr: os.Getenv("FRP_SERVER_ADDR"),
		ServerPort: envInt("FRP_SERVER_PORT", 7000),
		Token:      os.Getenv("FRP_TOKEN"),
		RemotePort: envInt("FRP_REMOTE_PORT", 0),
		Protocol:   envStr("FRP_PROTOCOL", "tcp"),
		BinaryPath: envStr("FRP_BINARY_PATH", "frpc"),
		LocalPort:  envInt("FRP_LOCAL_PORT", 5244),
	}
}

// envStr returns the env var value or def when unset/empty.
func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt parses an int env var, falling back to def when unset or malformed.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// validate checks the minimum fields needed to start a tunnel.
func (c frpConfig) validate() error {
	if c.ServerAddr == "" {
		return errors.New("frp: server address is required")
	}
	if c.RemotePort <= 0 {
		return errors.New("frp: remote port must be greater than 0")
	}
	return nil
}

// The frpc.toml v1 schema (github.com/fatedier/frp). Every field is written
// through the toml encoder so config VALUES (server address, token) are escaped
// by the encoder — never string-concatenated — which is what stops a malicious
// setting value from breaking out of its string and injecting extra toml
// (design §4 "toml/命令注入").
type frpcToml struct {
	ServerAddr string          `toml:"serverAddr"`
	ServerPort int             `toml:"serverPort"`
	Auth       frpcAuth        `toml:"auth"`
	Proxies    []frpcProxyToml `toml:"proxies"`
}

type frpcAuth struct {
	Method string `toml:"method"`
	Token  string `toml:"token"`
}

type frpcProxyToml struct {
	Name       string `toml:"name"`
	Type       string `toml:"type"`
	LocalIP    string `toml:"localIP"`
	LocalPort  int    `toml:"localPort"`
	RemotePort int    `toml:"remotePort"`
}

// renderFrpcToml encodes c into a frpc v1 config document. The single proxy maps
// 127.0.0.1:LocalPort (the alist HTTP port) to the frps RemotePort.
func renderFrpcToml(c frpConfig) ([]byte, error) {
	doc := frpcToml{
		ServerAddr: c.ServerAddr,
		ServerPort: c.ServerPort,
		Auth: frpcAuth{
			Method: "token",
			Token:  c.Token,
		},
		Proxies: []frpcProxyToml{
			{
				Name:       "alist",
				Type:       c.Protocol,
				LocalIP:    "127.0.0.1",
				LocalPort:  c.LocalPort,
				RemotePort: c.RemotePort,
			},
		},
	}
	return toml.Marshal(doc)
}
