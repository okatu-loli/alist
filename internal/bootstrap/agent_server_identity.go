package bootstrap

import (
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/agentsrv"
	log "github.com/sirupsen/logrus"
)

// InitServerIdentity loads (or creates on first boot) the server's long-term
// ed25519 identity for the reverse-connection feature (design R1) and installs
// it into the process-wide injection seam the anonymous pairing handler reads
// through (agentsrv.CurrentServerIdentity), so pair/complete can deliver the
// server's public key for an agent to pin. It is persisted under flags.DataDir
// — the same base every other piece of server-side state uses (frp/backup/the
// plugin bridge) — at agent_server_identity.json (0600, atomic).
//
// Wiring shape mirrors InitPluginHostBridge: a bootstrap func called from
// cmd/common.go that installs a singleton on a LOWER package (here the seam
// lives in internal/agentsrv — the server-side split of internal/agent, see
// the W1 repo-split analysis) so server/handles never imports bootstrap. On a
// load/create failure (e.g. an unwritable data dir, or a present-but-corrupt
// key file — which fails closed rather than silently regenerating and rotating
// the identity) the reverse identity is simply left unwired: pairing still
// works, it just omits server_pubkey, exactly as in a minimal boot. The
// private key is never logged.
func InitServerIdentity() {
	id, err := agentsrv.LoadOrCreateServerIdentity(flags.DataDir)
	if err != nil {
		log.Errorf("server identity: load/create failed: %+v (pairing will omit server_pubkey this run)", err)
		return
	}
	agentsrv.SetServerIdentity(id)
}
