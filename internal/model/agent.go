package model

import (
	"encoding/json"
	"strings"
	"time"
)

// Agent is a remote-agent record (design W12): a companion process an admin
// pairs to this alist instance so it can act as an extra endpoint for
// cross-storage transfers (the actual channel/task machinery lands in later
// waves — W12-2/3). This wave (W12-1) is the bounded data-layer + admin
// management foundation: the pairing HANDSHAKE that fills in AgentID/PubKey
// and actually creates a row happens in W12-2, so every field here must
// tolerate the zero value.
//
// AgentID is the stable identifier the agent self-reports at pairing time
// (a fingerprint of its long-term public key) — empty/unset this wave since
// no code path yet creates a row through a real pairing. PubKey is excluded
// from JSON (`json:"-"`) the same way model.AppPassword excludes Hash/Salt:
// it is authentication material, never meant to round-trip through an API
// response, even to an admin.
type Agent struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	AgentID string `json:"agent_id" gorm:"uniqueIndex"`
	Name    string `json:"name"`
	PubKey  string `json:"-"`
	Enabled bool   `json:"enabled"`
	// StorageWhitelist is a JSON-encoded []string of mount paths / storage ids
	// this agent may act against. Stored as raw text (gorm:"type:text") and
	// never hand-decoded by callers — use Whitelist()/SetWhitelist(). Empty
	// column ("" or "[]") both mean "no access", the safe default.
	StorageWhitelist string     `json:"storage_whitelist" gorm:"type:text"`
	LastSeen         *time.Time `json:"last_seen"`
	CreatedAt        time.Time  `json:"created_at"`
	// Confirmed marks that an admin has visually matched this agent's SAS
	// (design W12-2b) against what the agent's own terminal displayed and
	// affirmatively confirmed it (POST /admin/agents/:id/confirm) — the
	// step that flips Enabled to true for a freshly (re-)paired agent. It is
	// deliberately a SEPARATE field from Enabled, not a synonym for it: an
	// admin can still use PATCH /admin/agents/:id {enabled:false} to
	// administratively disable an already-confirmed agent at any time
	// (see UpdateAgent's doc) without that ever being mistaken for "still
	// awaiting its first SAS confirmation".
	Confirmed bool `json:"confirmed"`
	// Mode selects how the server and this agent reach each other (design R4).
	// "outbound" (the default, and every pre-R4 row via the 0018 backfill) is
	// the original W12 model: the agent dials the server and polls over HTTP.
	// "reverse" is the NAT-inverted model: the server DIALS this agent's
	// ReverseAddr WebSocket listener and holds a persistent bidirectional
	// channel. An empty Mode is treated as "outbound" everywhere (IsReverse),
	// so a zero value is always the safe, unchanged behavior.
	Mode string `json:"mode"`
	// ReverseAddr is the agent's dialable WebSocket URL for reverse mode — e.g.
	// "wss://vps.example:5343/agentchannel" (or ws:// on a trusted network).
	// Meaningful only when Mode=="reverse"; the server's reverse supervisor
	// dials exactly this URL. Empty in outbound mode.
	ReverseAddr string `json:"reverse_addr,omitempty"`
	// PendingSAS is the space-joined emoji sequence (see agent.DeriveSAS)
	// this row's most recent pair/complete derived, kept around ONLY until
	// Confirmed flips true (confirm clears it back to "") so a durable
	// pending-confirm state survives a server restart mid-pairing — an
	// admin's GET /admin/agents/:id/pairing-sas reads straight off this
	// column rather than an in-memory store. Excluded from the default DTO
	// (json:"-") because the *admin management* view (AgentDTO) is not
	// where a pairing SAS belongs — it is served only by the dedicated
	// pairing-sas endpoint, alongside Confirmed, via PendingSASList().
	PendingSAS string `json:"-" gorm:"type:text"`
}

// AgentModeOutbound is the default connection mode: the agent dials the server
// and polls over HTTP (the original W12 model). An empty Mode means this.
const AgentModeOutbound = "outbound"

// AgentModeReverse is the NAT-inverted mode: the server dials the agent's
// ReverseAddr WebSocket listener and holds a persistent channel (design R4).
const AgentModeReverse = "reverse"

// IsReverse reports whether this agent runs in reverse (server-dials-agent)
// mode. An empty Mode is outbound (the safe default), so only an explicit
// "reverse" flips it — a zero-value row is never accidentally reverse.
func (a *Agent) IsReverse() bool {
	return a.Mode == AgentModeReverse
}

// Whitelist decodes StorageWhitelist into a slice of mount paths / storage
// ids. An empty or malformed column decodes to an empty, non-nil slice — a
// freshly-paired agent with no granted storage is the safe default, not an
// error a caller must handle.
func (a *Agent) Whitelist() []string {
	if a.StorageWhitelist == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(a.StorageWhitelist), &out); err != nil {
		return []string{}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// SetWhitelist encodes paths into StorageWhitelist for storage. A nil or
// empty slice serializes to "[]" (not ""), so Whitelist() always has a JSON
// array to round-trip rather than relying on the empty-string special case.
func (a *Agent) SetWhitelist(paths []string) {
	if len(paths) == 0 {
		a.StorageWhitelist = "[]"
		return
	}
	b, err := json.Marshal(paths)
	if err != nil {
		a.StorageWhitelist = "[]"
		return
	}
	a.StorageWhitelist = string(b)
}

// PendingSASList splits PendingSAS back into the emoji slice agent.DeriveSAS
// produced. A space-joined string (not JSON) is deliberate: every entry is a
// single emoji from a fixed alphabet that never itself contains a space, so
// strings.Fields round-trips it exactly with no encoding/decoding
// machinery. An empty column (no pending confirmation, or already
// confirmed and cleared) decodes to an empty, non-nil slice.
func (a *Agent) PendingSASList() []string {
	fields := strings.Fields(a.PendingSAS)
	if fields == nil {
		fields = []string{}
	}
	return fields
}

// SetPendingSAS encodes sas into PendingSAS for storage. Passing nil/empty
// clears it (stores "") — this is exactly what confirming a pairing does:
// once Confirmed is true there is nothing left pending to show.
func (a *Agent) SetPendingSAS(sas []string) {
	a.PendingSAS = strings.Join(sas, " ")
}
