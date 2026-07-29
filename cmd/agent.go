package cmd

import (
	"github.com/AlistGo/alist-agent/cli"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/pkg/utils"
)

// The remote-agent CLI (join/run/serve) itself lives in the standalone
// github.com/AlistGo/alist-agent module's cli package (design W5) so it
// can be built and released independently of the main alist binary (see
// agent/cmd/alist-agent for that standalone entry point). This is just the
// thin shell that mounts the same command tree as `alist agent` here too
// (design R11: dual entry points, not a breaking move) — every flag/help
// text/behavior lives in agent/cli/agent.go, unchanged.
func init() {
	RootCmd.AddCommand(cli.NewAgentCmd(cli.BuildInfo{Version: conf.Version}, utils.Log))
}
