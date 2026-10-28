//go:build unix

package copilot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// TestMCPConfigPathByLaunchTarget asserts that a local runtime receives the
// host path of the configuration derived from the generated one. A runtime
// in an execution environment receives its servers inline instead (see
// TestCarrierTurnReachesTheToolServerOverItsChannel).
func TestMCPConfigPathByLaunchTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		target   string
		wantPath func(ws string) string
	}{
		{name: "local passes the host path", target: "local", wantPath: func(ws string) string { return filepath.Join(ws, ".sortie", derivedConfigName) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
				AgentConfig:   domain.AgentConfig{Command: fakeCopilotBinary(t)},
				MCPConfigPath: mcpPath,
			}

			a, _ := NewCopilotAdapter()
			session, err := a.StartSession(context.Background(), params)
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			state := session.Internal.(*sessionState)
			args := buildArgs(state, 1, "do work", passthroughConfig{})
			i := slices.Index(args, "--additional-mcp-config")
			if want := "@" + tt.wantPath(ws); i < 0 || i+1 >= len(args) || args[i+1] != want {
				t.Errorf("buildArgs() = %q, want --additional-mcp-config %q", args, want)
			}
		})
	}
}
