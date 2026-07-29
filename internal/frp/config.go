package frp

import (
	"errors"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
)

// frpConfig is the resolved frp tunnel configuration read from settings plus the
// local HTTP port to expose. It is a plain value: load() reads the current
// settings each time so a config change is picked up on the next (re)start.
type frpConfig struct {
	Enabled    bool
	ServerAddr string
	ServerPort int
	Token      string
	RemotePort int
	Protocol   string
	BinaryPath string
	LocalPort  int
}

// loadFrpConfig reads the frp settings and pins LocalPort to the running HTTP
// port (127.0.0.1:HttpPort is the only thing the tunnel exposes — design §4).
func loadFrpConfig() frpConfig {
	protocol := setting.GetStr(conf.FrpProtocol, "tcp")
	binary := setting.GetStr(conf.FrpBinaryPath, "frpc")
	return frpConfig{
		Enabled:    setting.GetBool(conf.FrpEnabled),
		ServerAddr: setting.GetStr(conf.FrpServerAddr),
		ServerPort: setting.GetInt(conf.FrpServerPort, 7000),
		Token:      setting.GetStr(conf.FrpToken),
		RemotePort: setting.GetInt(conf.FrpRemotePort, 0),
		Protocol:   protocol,
		BinaryPath: binary,
		LocalPort:  conf.Conf.Scheme.HttpPort,
	}
}

// validate checks the minimum fields needed to start a tunnel. It is only
// meaningful when Enabled — a disabled tunnel never reaches validation.
func (c frpConfig) validate() error {
	if c.ServerAddr == "" {
		return errors.New("frp: server address is required")
	}
	if c.RemotePort <= 0 {
		return errors.New("frp: remote port must be greater than 0")
	}
	return nil
}
