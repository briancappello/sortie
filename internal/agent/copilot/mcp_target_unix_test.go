//go:build unix

package copilot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestMCPConfigPathByLaunchTarget asserts, for each launch target, which
// MCP configuration path the runtime receives: the host path on this
// host and over SSH, and the path at which an execution environment's
// runtime sees the same file.
func TestMCPConfigPathByLaunchTarget(t *testing.T) {
	// Not parallel: the SSH case puts a fake ssh first on PATH.
	tests := []struct {
		name     string
		target   string
		wantPath func(ws string) string
	}{
		{name: "local passes the host path", target: "local", wantPath: func(ws string) string { return filepath.Join(ws, ".sortie", "mcp.json") }},
		{name: "ssh passes the host path", target: "ssh", wantPath: func(ws string) string { return filepath.Join(ws, ".sortie", "mcp.json") }},
		{name: "environment carrier passes the guest path", target: "carrier", wantPath: func(string) string { return "/workspace/.sortie/mcp.json" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := t.TempDir()
			mcpPath := filepath.Join(ws, ".sortie", "mcp.json")
			params := domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   domain.AgentConfig{Command: fakeCopilotBinary(t)},
				MCPConfigPath: mcpPath,
			}
			switch tt.target {
			case "ssh":
				sshDir := t.TempDir()
				agenttest.WriteScript(t, sshDir, "ssh", "exit 0\n")
				t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				params.SSHHost = "build-host"
			case "carrier":
				params.Carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}
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
