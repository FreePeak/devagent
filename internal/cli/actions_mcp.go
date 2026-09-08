package cli

import (
	"github.com/FreePeak/devagent/internal/server/mcp"
	"github.com/spf13/cobra"
)

// mcpCommand mirrors `devagent mcp` (src/cli.ts:1277): start the stdio MCP
// server — JSON-RPC 2.0, one message per line on stdin/stdout — exposing
// DevAgent as tools (devagent_dispatch/status/log/board/ledger/answer) for
// MCP-capable hosts. The server serves until stdin closes; #251.
func mcpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Expose DevAgent as MCP tools over stdio (devagent_dispatch/status/log)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return mcp.Run()
		},
	}
}
