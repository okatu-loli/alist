package hostcap

import (
	"context"
	"crypto/rand"
	"io"

	ext "github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/crypto"
)

// Host is the concrete ext.Host handed to a plugin, bound to that plugin's id +
// Grant. Every capability accessor returns an object that enforces the plugin's
// grant and the matching security invariant (see grant.go). Construct one per
// plugin; do not share across plugins (Store/Cache namespacing and the FS
// principal are all derived from this plugin's grant).
type Host struct {
	grant  Grant
	logger ext.Logger

	prin         *principals
	fs           *hostFS
	auth         *hostAuth
	store        *hostStore
	sign         *hostSign
	http         *hostHTTP
	cache        *hostCache
	configLoader func() ([]byte, error)
	configSaver  func([]byte) error
}

// Options configures the host-side backends a plugin's capabilities draw on.
type Options struct {
	// Logger is the plugin-namespaced logger (may be nil).
	Logger ext.Logger
	// KVBackend persists Store data. Defaults to a process-local MemKVBackend.
	KVBackend KVBackend
	// SecretsBackend persists Store().Secrets() data (already encrypting —
	// see NewEncryptingKVBackend). Defaults to an encrypting process-local
	// MemKVBackend under a fresh, never-persisted key: fine for tests and for
	// hosts that never restart with the same Options, but a caller that wants
	// secrets to survive a restart (the real bootstrap wiring) MUST pass one
	// built over a persisted key (hostcap.LoadOrCreateSecretsKey) and a
	// storage extent disjoint from KVBackend's.
	SecretsBackend KVBackend
	// OutboundPolicy is the global egress gate consulted before Grant.Network.
	// Defaults to a deny-all policy (privacy baseline: no outbound).
	OutboundPolicy OutboundPolicy
	// DelegatedOnly builds a Host for an UNAUTHENTICATED context (a plugin's
	// PublicPage / public route handler, design §2.5). When set, Host.FS refuses
	// the plugin's own bound/self principal (the empty Principal handle): a public
	// handler must act ONLY through an explicitly delegated principal it obtained
	// via Auth().DelegatedPrincipal (owner-scoped, subtree-anchored, time-limited).
	// This is what stops a public share landing page from bare-calling host.FS to
	// read arbitrary files the plugin was granted — a public route can never be a
	// backdoor to the plugin's own fs grant.
	DelegatedOnly bool
	// ConfigLoader reads the plugin's persisted config_form value as JSON. nil
	// (the default) means Host.LoadConfig always returns nil, nil.
	ConfigLoader func() ([]byte, error)
	// ConfigSaver persists the plugin's own config_form value as JSON. nil (the
	// default) means Host.SaveConfig refuses the write with ErrCodeNotImplement
	// instead of silently dropping it — unlike LoadConfig's nil,nil no-op read,
	// a save the caller believes succeeded but didn't is the worse failure mode.
	ConfigSaver func([]byte) error
}

// New builds the ext.Host for a plugin from its Grant and host-side options.
func New(grant Grant, opts Options) *Host {
	if opts.KVBackend == nil {
		opts.KVBackend = NewMemKVBackend()
	}
	if opts.SecretsBackend == nil {
		opts.SecretsBackend = mustEphemeralSecretsBackend()
	}
	if opts.OutboundPolicy == nil {
		// Fail-closed: with no policy installed, no host is permitted.
		opts.OutboundPolicy = OutboundPolicyFunc(func(string) bool { return false })
	}
	prin := newPrincipals()
	return &Host{
		grant:        grant,
		logger:       opts.Logger,
		prin:         prin,
		fs:           newHostFS(grant, prin, opts.DelegatedOnly),
		auth:         &hostAuth{grant: grant, prin: prin},
		store:        &hostStore{grant: grant, backend: opts.KVBackend, secretsBackend: opts.SecretsBackend},
		sign:         newHostSign(grant, prin),
		http:         newHostHTTP(grant, opts.OutboundPolicy),
		cache:        newHostCache(grant.HasAPI(CapCache)),
		configLoader: opts.ConfigLoader,
		configSaver:  opts.ConfigSaver,
	}
}

var _ ext.Host = (*Host)(nil)

func (h *Host) Logger() ext.Logger   { return h.logger }
func (h *Host) FS() ext.FS           { return h.fs }
func (h *Host) Auth() ext.Auth       { return h.auth }
func (h *Host) Store() ext.Store     { return h.store }
func (h *Host) Sign() ext.Signer     { return h.sign }
func (h *Host) HTTP() ext.HTTPClient { return h.http }
func (h *Host) Cache() ext.Cache     { return h.cache }

// LoadConfig returns the plugin's persisted config as JSON via the installed
// ConfigLoader, or nil, nil if no loader was configured.
func (h *Host) LoadConfig(ctx context.Context) ([]byte, error) {
	if h.configLoader == nil {
		return nil, nil
	}
	return h.configLoader()
}

// errNoConfigSaver is returned by SaveConfig when no ConfigSaver was wired.
// Unlike LoadConfig's nil-loader no-op read (there is nothing to lose by
// returning nil,nil), silently discarding a save would make the caller
// believe its config persisted when it did not — the safer failure mode is an
// explicit, documented error.
var errNoConfigSaver = ext.NewError(ext.ErrCodeNotImplement, "hostcap: no config saver configured for this plugin")

// SaveConfig persists the plugin's own config as JSON via the installed
// ConfigSaver. With no saver configured, it returns errNoConfigSaver rather
// than a silent no-op (mirrors LoadConfig's leg in the write direction).
func (h *Host) SaveConfig(ctx context.Context, cfg []byte) error {
	if h.configSaver == nil {
		return errNoConfigSaver
	}
	return h.configSaver(cfg)
}

// Capabilities returns the names of the Host APIs this plugin's Grant
// authorizes (introspection over h.grant.HostAPIs), in grant order. An empty
// grant yields an empty (nil) slice, never an error.
func (h *Host) Capabilities(ctx context.Context) ([]string, error) {
	if len(h.grant.HostAPIs) == 0 {
		return nil, nil
	}
	names := make([]string, len(h.grant.HostAPIs))
	for i, it := range h.grant.HostAPIs {
		names[i] = it.Name
	}
	return names, nil
}

// Grant returns the plugin's effective grant (read-only view for the reverse
// bridge server, which forwards its identity into the HostService handlers).
func (h *Host) Grant() Grant { return h.grant }

// PreAuthorizePut lets the reverse-bridge FSPut receiver authorize a destination
// WRITE before it spools any bytes (HIGH-2). It is not part of the ext.Host
// interface (a plugin never calls it); the receiver type-asserts for it.
func (h *Host) PreAuthorizePut(ctx context.Context, p ext.Principal, dstDir ext.ObjRef) error {
	return h.fs.PreAuthorizePut(ctx, p, dstDir)
}

// mustEphemeralSecretsBackend builds the default Options.SecretsBackend: an
// encrypting wrapper over a fresh MemKVBackend, keyed by a random 32-byte key
// that is never persisted (so it does not survive process restart — callers
// that need persistence must pass their own SecretsBackend, see the Options
// doc). Reading crypto/rand can only fail if the OS entropy source itself is
// broken, which is not a recoverable condition for a security primitive — a
// panic here surfaces immediately rather than silently degrading to a weak or
// predictable key.
func mustEphemeralSecretsBackend() KVBackend {
	key := make([]byte, crypto.KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		panic("hostcap: failed to read random secrets key: " + err.Error())
	}
	b, err := NewEncryptingKVBackend(NewMemKVBackend(), key)
	if err != nil {
		// Unreachable: key is always crypto.KeySize bytes.
		panic("hostcap: " + err.Error())
	}
	return b
}
