package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AlistGo/alist-agent/proto"
)

// identityFileName is the file an agent's long-term keypair is persisted
// under, relative to its --data directory.
const identityFileName = "agent_identity.json"

// Identity is an agent's long-term ed25519 keypair — the credential every
// heartbeat proves possession of (see proto.HeartbeatMessage). PrivKey never
// leaves this struct / this process: it is never part of any wire message
// (PairCompleteRequest sends only the public key) and must never be logged.
type Identity struct {
	PubKey  ed25519.PublicKey
	PrivKey ed25519.PrivateKey
}

// AgentID returns this identity's stable fingerprint (proto.FingerprintPubKey) —
// the id the server will assign once a pairing using this key completes.
func (id *Identity) AgentID() string {
	return proto.FingerprintPubKey(id.PubKey)
}

// Sign proves possession of PrivKey over msg (see proto.HeartbeatMessage).
func (id *Identity) Sign(msg []byte) []byte {
	return ed25519.Sign(id.PrivKey, msg)
}

// identityFile is the on-disk JSON shape: raw key bytes, base64-std encoded.
type identityFile struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

// LoadOrCreateIdentity loads the ed25519 identity persisted under dir, or
// generates and persists a fresh one (via crypto/ed25519 + crypto/rand) if
// none exists yet. dir is created (0700) if missing; the identity file
// itself is written at 0600 via an atomic (temp-file + rename) write so a
// crash mid-write never leaves a corrupt or briefly-world-readable key file
// behind. Reusing an existing file is what gives a restarted or re-run
// agent a stable AgentID rather than minting a new identity (and thus a new
// AgentID) every time.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("agent: create data dir: %w", err)
	}
	path := filepath.Join(dir, identityFileName)
	b, err := os.ReadFile(path)
	if err == nil {
		return parseIdentityFile(b)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("agent: read identity file: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("agent: generate identity: %w", err)
	}
	id := &Identity{PubKey: pub, PrivKey: priv}
	if err := saveIdentity(dir, id); err != nil {
		return nil, err
	}
	return id, nil
}

func parseIdentityFile(b []byte) (*Identity, error) {
	var f identityFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("agent: parse identity file: %w", err)
	}
	pub, err := base64.StdEncoding.DecodeString(f.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("agent: identity file has a malformed public key")
	}
	priv, err := base64.StdEncoding.DecodeString(f.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("agent: identity file has a malformed private key")
	}
	return &Identity{PubKey: ed25519.PublicKey(pub), PrivKey: ed25519.PrivateKey(priv)}, nil
}

func saveIdentity(dir string, id *Identity) error {
	f := identityFile{
		PublicKey:  base64.StdEncoding.EncodeToString(id.PubKey),
		PrivateKey: base64.StdEncoding.EncodeToString(id.PrivKey),
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("agent: encode identity: %w", err)
	}
	return atomicWriteFile(filepath.Join(dir, identityFileName), b, 0600)
}

// atomicWriteFile writes b to path via a temp file created in the same
// directory (so the final rename is same-filesystem/atomic) followed by a
// rename. The temp file is created with the FINAL permission mode before
// any bytes land, so sensitive content (an identity's private key) is never
// briefly readable under a more permissive default mode.
func atomicWriteFile(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("agent: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("agent: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("agent: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("agent: rename temp file: %w", err)
	}
	return nil
}
