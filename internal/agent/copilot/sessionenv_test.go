package copilot

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestSessionEnvReachesEveryLaunch asserts that SORTIE_ISSUE_ID from the
// session environment reaches every process the session launches: each
// turn and the auxiliary server that deletes a verification session.
// The local version canary checks the binary before any session exists
// and is not a session launch.
func TestSessionEnvReachesEveryLaunch(t *testing.T) {
	// Not parallel: t.Setenv carries the stale inherited value.
	t.Setenv("SORTIE_ISSUE_ID", "stale")

	tests := []struct {
		name   string
		launch func(t *testing.T, adapter *CopilotAdapter, session domain.Session, ctx context.Context)
		match  func(agenttest.EnvProbeLaunch) bool
	}{
		{
			name: "turn",
			launch: func(_ *testing.T, adapter *CopilotAdapter, session domain.Session, ctx context.Context) {
				_, _ = adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}})
			},
			match: func(l agenttest.EnvProbeLaunch) bool { return !slices.Contains(l.Args, "--version") },
		},
		{
			name: "verification session delete",
			launch: func(_ *testing.T, _ *CopilotAdapter, session domain.Session, ctx context.Context) {
				state := session.Internal.(*sessionState)
				deleteVerificationSession(ctx, state.target, "sess-1", time.Minute, 0, slog.New(slog.DiscardHandler))
			},
			match: func(l agenttest.EnvProbeLaunch) bool { return slices.Contains(l.Args, "--server") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.jsonl")
			command := agenttest.FakeRuntime(t, dir, "copilot", agenttest.EnvProbeScenario, agenttest.EnvProbe{
				Path: probe, Names: []string{"SORTIE_ISSUE_ID"}, Version: "1.0.0",
			})
			adapter, err := NewCopilotAdapter()
			if err != nil {
				t.Fatalf("NewCopilotAdapter() error = %v", err)
			}
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig:   domain.AgentConfig{Command: command},
				SessionEnv:    []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "42"}},
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			copilotAdapter, ok := adapter.(*CopilotAdapter)
			if !ok {
				t.Fatalf("NewCopilotAdapter() = %T, want *CopilotAdapter", adapter)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				tt.launch(t, copilotAdapter, session, ctx)
			}()

			launch := agenttest.AwaitEnvProbe(t, probe, tt.match)
			if got := launch.Env["SORTIE_ISSUE_ID"]; got != "42" {
				t.Errorf("launched %v with SORTIE_ISSUE_ID = %q, want %q", launch.Args, got, "42")
			}
		})
	}
}
