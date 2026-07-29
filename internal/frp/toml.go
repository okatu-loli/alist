package frp

import (
	toml "github.com/pelletier/go-toml/v2"
)

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
