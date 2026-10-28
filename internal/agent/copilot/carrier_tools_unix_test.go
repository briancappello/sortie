//go:build unix

package copilot

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestCarrierTurnReachesTheToolServerOverItsChannel asserts that a session
// in an execution environment declares Sortie's tool server inline, at the
// tool channel's guest port, with no configuration file path, and that the
// bearer reaches the agent by name only.
func TestCarrierTurnReachesTheToolServerOverItsChannel(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	carrier := &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace", Channel: agenttest.NewToolChannel(t)}
	params := domain.StartSessionParams{
		WorkspacePath: ws,
		AgentConfig:   domain.AgentConfig{Command: fakeCopilotBinary(t)},
		MCPConfigPath: agenttest.WriteHTTPToolServerConfig(t, ws),
		Carrier:       carrier,
	}

	a, _ := NewCopilotAdapter()
	session, err := a.StartSession(context.Background(), params)
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })
	_, _ = a.RunTurn(context.Background(), session, domain.RunTurnParams{Prompt: "do work", OnEvent: func(domain.AgentEvent) {}})

	launches := carrier.Launches()
	i := slices.IndexFunc(launches, func(l domain.CarrierLaunch) bool {
		return strings.Contains(strings.Join(l.Argv, " "), "http://127.0.0.1:47800/mcp")
	})
	if i < 0 {
		t.Fatalf("carrier launches = %+v, want a turn declaring Sortie's tool server at the guest port", launches)
	}
	turn := launches[i]
	argv := strings.Join(turn.Argv, " ")
	if strings.Contains(argv, ".sortie/") {
		t.Errorf("turn argv = %q, want no configuration file path: the guest reads none", turn.Argv)
	}
	if !slices.Contains(turn.SecretEnv, mcpconfig.BearerEnv) {
		t.Errorf("turn SecretEnv = %q, want %s passed by name", turn.SecretEnv, mcpconfig.BearerEnv)
	}
}
