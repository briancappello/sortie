//go:build unix

package opencode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// TestMCPServersByLaunchTarget asserts, for each launch target, whether
// the session passes the generated MCP servers to the runtime. opencode
// inlines them from the host copy of the file into the turn's
// configuration, so the path itself never reaches the runtime.
func TestMCPServersByLaunchTarget(t *testing.T) {
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
			dir := filepath.Join(ws, workspacekit.SortieDir)
			if err := os.Mkdir(dir, 0o750); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
			mcpPath := filepath.Join(dir, "mcp.json")
			if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"sortie-tools":{"command":"/usr/local/bin/sortie","args":["mcp-server"]}}}`), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			command := fakeMinimalRuntime(t)
			params := domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   domain.AgentConfig{Command: command},
				MCPConfigPath: mcpPath,
			}
			var carrier *agenttest.FakeCarrier
			switch tt.target {
			case "ssh":
				sshDir := t.TempDir()
				agenttest.FakeRuntime(t, sshDir, "ssh", agenttest.OutputScenario, agenttest.Output{Stdout: "opencode v2.0.18\n"})
				t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				params.SSHHost = "build-host"
			case "carrier":
				carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}
				params.Carrier = carrier
			}

			a, _ := NewOpenCodeAdapter()
			session, err := a.StartSession(context.Background(), params)
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			state := session.Internal.(*sessionState)
			doc := decodeConfigDocument(t, state.turnConfigContent)
			if _, ok := doc["mcp"]; ok != tt.wantMCP {
				t.Errorf("turn configuration mcp present = %v, want %v (content %q)", ok, tt.wantMCP, state.turnConfigContent)
			}
			if tt.wantMCP && !strings.Contains(string(doc["mcp"]), "sortie-tools") {
				t.Errorf("turn configuration mcp = %s, want the servers read from the host copy %s", doc["mcp"], mcpPath)
			}
			if carrier == nil {
				return
			}

			// The turn reaches the runtime through the carrier, which carries
			// the configuration as an agent variable.
			_, _ = a.RunTurn(context.Background(), session, domain.RunTurnParams{Prompt: "hi", OnEvent: func(domain.AgentEvent) {}})
			launches := carrier.Launches()
			if len(launches) != 2 {
				t.Fatalf("carrier launches = %d, want the version query and the turn", len(launches))
			}
			if got := launches[0].Argv; !slices.Equal(got, []string{command, "--version"}) {
				t.Errorf("version query argv = %q, want %q", got, []string{command, "--version"})
			}
			turn := launches[1]
			if turn.Argv[0] != command {
				t.Errorf("turn argv = %q, want it to run %q", turn.Argv, command)
			}
			idx := slices.IndexFunc(turn.AgentEnv, func(e string) bool { return strings.HasPrefix(e, "OPENCODE_CONFIG_CONTENT=") })
			if idx < 0 || !strings.Contains(turn.AgentEnv[idx], "sortie-tools") {
				t.Errorf("turn agent env = %q, want OPENCODE_CONFIG_CONTENT naming the MCP servers", turn.AgentEnv)
			}
		})
	}
}
