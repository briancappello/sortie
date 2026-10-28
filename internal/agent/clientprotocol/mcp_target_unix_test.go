//go:build unix

package clientprotocol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// TestMCPServersByLaunchTarget asserts, for each remote launch target,
// whether session/new hands the runtime Sortie's tool server: none over
// SSH without a forward, and over HTTP at the tool channel's guest port in
// an execution environment. The protocol carries servers inline, so no
// configuration path reaches the runtime. A local launch is covered by the
// shared tool-delivery tests.
func TestMCPServersByLaunchTarget(t *testing.T) {
	// Not parallel: the SSH case puts a fake ssh first on PATH.
	tests := []struct {
		name    string
		target  string
		wantMCP bool
	}{
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
				carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace", Channel: agenttest.NewToolChannel(t)}
				params.Carrier = carrier
				params.MCPConfigPath = agenttest.WriteHTTPToolServerConfig(t, ws)
				// The stand-in agent never connects; the session gives up
				// waiting for it after the read timeout.
				params.AgentConfig.ReadTimeoutMS = 200
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
			if carrier != nil && !strings.Contains(string(captured), "http://127.0.0.1:47800/mcp") {
				t.Errorf("session/new = %s, want Sortie's tool server at the tool channel's guest port", captured)
			}
			if carrier != nil && len(carrier.Launches()) != 1 {
				t.Errorf("carrier launches = %d, want the session's one launch", len(carrier.Launches()))
			}
		})
	}
}

// TestSessionCwdByLaunchTarget asserts that session/new names the
// workspace as the runtime sees it: the host path for a local launch,
// and the guest path for a launch through an execution environment,
// where the host path does not exist. The credential verification
// session, which carries no tool servers, names the same path.
func TestSessionCwdByLaunchTarget(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"local", "carrier"} {
		for _, verification := range []bool{false, true} {
			// The fake carrier runs the agent through env(1), which would
			// read a temporary path holding '=' as an assignment.
			name := target + " session"
			if verification {
				name = target + " credential verification"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				ws := t.TempDir()
				dir := t.TempDir()
				capturePath := filepath.Join(dir, "session_new.jsonl")
				params := domain.StartSessionParams{
					WorkspacePath:          ws,
					AgentConfig:            domain.AgentConfig{Command: agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))},
					CredentialVerification: verification,
				}
				wantCwd := ws
				if target == "carrier" {
					params.Carrier = &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}
					wantCwd = "/workspace"
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
				var request struct {
					Params struct {
						Cwd string `json:"cwd"`
					} `json:"params"`
				}
				if err := json.Unmarshal(captured, &request); err != nil {
					t.Fatalf("decode session/new %s: %v", captured, err)
				}
				if request.Params.Cwd != wantCwd {
					t.Errorf("session/new cwd = %q, want %q", request.Params.Cwd, wantCwd)
				}
			})
		}
	}
}
