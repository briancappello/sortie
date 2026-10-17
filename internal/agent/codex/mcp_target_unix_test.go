//go:build unix

package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestMCPOverridesByLaunchTarget asserts, for each launch target, whether
// the session passes the generated MCP servers to the runtime. codex
// renders them from the host copy of the file as override arguments, so
// the path itself never reaches the runtime; a carried launch runs those
// arguments where the environment runs the runtime.
func TestMCPOverridesByLaunchTarget(t *testing.T) {
	// Not parallel: the SSH case puts a fake ssh first on PATH.
	tests := []struct {
		name    string
		target  string
		wantMCP bool
	}{
		{name: "local", target: "local", wantMCP: true},
		{name: "ssh gets no servers", target: "ssh"},
		{name: "environment carrier gets the servers", target: "carrier", wantMCP: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := t.TempDir()
			mcpPath := filepath.Join(ws, ".sortie", "mcp.json")
			if err := os.MkdirAll(filepath.Dir(mcpPath), 0o750); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server"]}}}`), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			params := domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   domain.AgentConfig{Command: "sh -c"},
				MCPConfigPath: mcpPath,
			}
			var carrier *agenttest.FakeCarrier
			switch tt.target {
			case "ssh":
				sshDir := t.TempDir()
				agenttest.WriteScript(t, sshDir, "ssh", "exit 0\n")
				t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				params.SSHHost = "build-host"
			case "carrier":
				carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}
				params.Carrier = carrier
			}

			target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
			if agentErr != nil {
				t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
			}
			args, agentErr := mcpOverrideArgs(target, params.MCPConfigPath)
			if agentErr != nil {
				t.Fatalf("mcpOverrideArgs() error = %v", agentErr)
			}
			hasServer := slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "mcp_servers.sortie-tools=") })
			if hasServer != tt.wantMCP {
				t.Errorf("mcpOverrideArgs() = %q, sortie-tools override present = %v, want %v", args, hasServer, tt.wantMCP)
			}
			if carrier == nil {
				return
			}

			target.Args = append(target.Args, args...)
			if _, agentErr := target.RemoteLaunch(nil); agentErr != nil {
				t.Fatalf("RemoteLaunch() error = %v", agentErr)
			}
			launches := carrier.Launches()
			if len(launches) != 1 || !slices.Equal(launches[0].Argv[len(launches[0].Argv)-len(args):], args) {
				t.Errorf("carrier launches = %+v, want the agent argv to end with the overrides %q", launches, args)
			}
		})
	}
}
