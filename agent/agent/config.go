package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// configFileName is the file `alist agent join` persists once pairing
// succeeds, relative to the agent's --data directory.
const configFileName = "agent_config.json"

// ClientConfig is what a later `alist agent run` needs to know: which
// server to call and which AgentID the server assigned this identity at
// pairing time. No secrets live here (that's agent_identity.json) — host,
// agent_id, and the server's PUBLIC key are not sensitive.
//
// ServerPubKey is the server's ed25519 public key (base64) as delivered on
// the pair/complete response (design R1). It is PINNED here at join time so a
// later reverse-connection wave (R2) can verify the server's signed handshake
// against it — the mirror image of the server verifying an agent via its
// pinned pubkey. This wave only STORES it; nothing verifies against it yet.
// Empty when pairing against a server that has no reverse identity wired.
type ClientConfig struct {
	Host         string `json:"host"`
	AgentID      string `json:"agent_id"`
	ServerPubKey string `json:"server_pubkey,omitempty"`
}

// SaveClientConfig persists cfg to <dir>/agent_config.json via the same
// atomic (temp-file + rename) write LoadOrCreateIdentity uses, so a crash
// mid-write never leaves a truncated config behind.
func SaveClientConfig(dir string, cfg ClientConfig) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("agent: create data dir: %w", err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("agent: encode config: %w", err)
	}
	return atomicWriteFile(filepath.Join(dir, configFileName), b, 0600)
}

// LoadClientConfig reads a previously saved config. A missing or
// incomplete file is a plain error — `alist agent run` turns it into a
// "run `alist agent join` first" message rather than trying to run
// half-configured.
//
// Host is deliberately NOT required here: a token-paired REVERSE agent
// (design F) stores agent_id + server_pubkey with no host — it never dials
// out, the server dials it. The outbound `agent run` path enforces Host at
// its call site.
func LoadClientConfig(dir string) (*ClientConfig, error) {
	b, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		return nil, fmt.Errorf("agent: read config: %w", err)
	}
	var cfg ClientConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("agent: parse config: %w", err)
	}
	if cfg.AgentID == "" {
		return nil, fmt.Errorf("agent: config is incomplete (missing agent_id)")
	}
	return &cfg, nil
}
