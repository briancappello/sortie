//go:build unix

package clientprotocol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// TestMCPServersByLaunchTarget asserts, for each launch target, whether
// session/new hands the runtime the generated MCP servers. The protocol
// carries them inline, parsed from the host copy of the file, so the
// path itself never reaches the runtime.
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
			sortieDir := filepath.Join(ws, workspacekit.SortieDir)
			if err := os.Mkdir(sortieDir, 0o750); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
			mcpPath := filepath.Join(sortieDir, "mcp.json")
			if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server"]}}}`), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			dir := t.TempDir()
			capturePath := filepath.Join(dir, "session_new.jsonl")
			params := domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   domain.AgentConfig{Command: agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))},
				MCPConfigPath: mcpPath,
			}
			var carrier *agenttest.FakeCarrier
			switch tt.target {
			case "ssh":
				sshDir := t.TempDir()
				agenttest.FakeRuntime(t, sshDir, "ssh", scenarioSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, false)})
				t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				params.SSHHost = "user@stand-in-host"
			case "carrier":
				carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}
				params.Carrier = carrier
			}

			session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, params)
			if err != nil {
				t.Fatalf("startSession() error = %v", err)
			}
			t.Cleanup(func() {
				if err := stopSession(context.Background(), session); err != nil {
					t.Errorf("stopSession() error = %v", err)
				}
			})

			captured, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("read captured session/new request: %v", err)
			}
			if got := strings.Contains(string(captured), "sortie-tools"); got != tt.wantMCP {
				t.Errorf("session/new names sortie-tools = %v, want %v: %s", got, tt.wantMCP, captured)
			}
			if carrier != nil && len(carrier.Launches()) != 1 {
				t.Errorf("carrier launches = %d, want the session's one launch", len(carrier.Launches()))
			}
		})
	}
}
