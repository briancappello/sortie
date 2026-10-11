package claude

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestSessionEnvReachesEveryLaunch asserts that SORTIE_ISSUE_ID from the
// session environment reaches every process this adapter launches, over
// a stale value the orchestrator inherited.
func TestSessionEnvReachesEveryLaunch(t *testing.T) {
	// Not parallel: t.Setenv carries the stale inherited value.
	t.Setenv("SORTIE_ISSUE_ID", "stale")

	tests := []struct {
		name         string
		verification bool
	}{
		{name: "work turn"},
		{name: "credential verification turn", verification: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.jsonl")
			command := agenttest.FakeRuntime(t, dir, "fake-claude", agenttest.EnvProbeScenario, agenttest.EnvProbe{
				Path: probe, Names: []string{"SORTIE_ISSUE_ID"},
			})
			adapter, err := NewClaudeCodeAdapter()
			if err != nil {
				t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
			}
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath:          t.TempDir(),
				AgentConfig:            domain.AgentConfig{Command: command},
				CredentialVerification: tt.verification,
				SessionEnv:             []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "42"}},
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				_, _ = adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}})
			}()

			launch := agenttest.AwaitEnvProbe(t, probe, func(agenttest.EnvProbeLaunch) bool { return true })
			if got := launch.Env["SORTIE_ISSUE_ID"]; got != "42" {
				t.Errorf("launched SORTIE_ISSUE_ID = %q, want %q", got, "42")
			}
		})
	}
}
