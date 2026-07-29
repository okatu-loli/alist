// Command alist-agent is the standalone remote-agent binary (design W5,
// R11 dual entry points): everything the `alist agent` subcommand exposes
// from the main AList binary (join/run/serve), built from the exact same
// github.com/AlistGo/alist-agent/cli command tree, but shippable and
// runnable without the main alist server binary at all.
package main

import (
	"os"

	"github.com/AlistGo/alist-agent/cli"
	"github.com/sirupsen/logrus"
)

// version is injected at build time via:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/alist-agent
//
// and defaults to "dev" for local/unreleased builds.
var version = "dev"

func main() {
	log := logrus.StandardLogger()

	root := cli.NewAgentCmd(cli.BuildInfo{Version: version}, log)
	// The agent subcommand tree is rooted at "agent" when mounted inside the
	// main alist binary (`alist agent join`); as a standalone binary its
	// own root command should read as `alist-agent join`, not
	// `alist-agent agent join`. Reuse the same tree — just relabel Use/Short
	// on this top-level instance so --help matches the standalone binary's
	// own name, while every subcommand (join/run/serve) and its flags/help
	// text stay byte-for-byte identical.
	root.Use = "alist-agent"
	root.Short = "Run or pair a remote alist agent"

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
