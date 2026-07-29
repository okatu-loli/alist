package bootstrap

import (
	"testing"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/conf"
)

// tokenstore's persistent revocation records are only as trustworthy as the
// JWT signature covering them: if JwtSecret changed on every restart, every
// outstanding token (and its store entry) would become unverifiable the
// moment the process restarted, silently defeating "survives restart".
// This pins the actual persistence path (InitConfig's create-then-reload)
// rather than re-deriving it from DefaultConfig alone.
func TestJwtSecretPersistsAcrossConfigReload(t *testing.T) {
	oldDataDir, oldConf := flags.DataDir, conf.Conf
	t.Cleanup(func() {
		flags.DataDir = oldDataDir
		conf.Conf = oldConf
	})
	flags.DataDir = t.TempDir()

	InitConfig()
	if conf.Conf.JwtSecret == "" {
		t.Fatal("first InitConfig must generate a non-empty JwtSecret")
	}
	firstSecret := conf.Conf.JwtSecret

	// simulate a restart: config.json now exists on disk, InitConfig must
	// read the same secret back rather than generating a new one.
	conf.Conf = nil
	InitConfig()
	if conf.Conf.JwtSecret != firstSecret {
		t.Fatalf("JwtSecret must be stable across restarts: got %q, want %q", conf.Conf.JwtSecret, firstSecret)
	}
}
