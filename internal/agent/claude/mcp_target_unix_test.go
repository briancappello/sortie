//go:build unix

package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestMCPConfigPathByLaunchTarget asserts which path of the configuration
// derived from the generated one the runtime receives: the host path on
// this host, and the path at which an execution environment's runtime sees
// the same file.
func TestMCPConfigPathByLaunchTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		target   string
		wantPath func(ws string) string
	}{
		{name: "local passes the host path", target: "local", wantPath: func(ws string) string { return filepath.Join(ws, ".sortie", mcpConfigName) }},
		{name: "environment carrier passes the guest path", target: "carrier", wantPath: func(string) string { return "/workspace/.sortie/" + mcpConfigName }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := t.TempDir()
			mcpPath := filepath.Join(ws, ".sortie", "mcp.json")
			if err := os.MkdirAll(filepath.Dir(mcpPath), 0o750); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/opt/sortie/sortie","args":["mcp-relay","/run/sortie/tools.sock"]}}}`), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			params := domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   domain.AgentConfig{Command: agenttest.WriteScript(t, t.TempDir(), "claude", "exit 0\n")},
				MCPConfigPath: mcpPath,
			}
			if tt.target == "carrier" {
				params.Carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}
			}

			a, _ := NewClaudeCodeAdapter()
			session, err := a.StartSession(context.Background(), params)
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			state := session.Internal.(*sessionState)
			args := buildArgs(state, 1, "do work", passthroughConfig{SessionPersistence: true})
			assertHasArgPair(t, args, "--mcp-config", tt.wantPath(ws))
		})
	}
}
