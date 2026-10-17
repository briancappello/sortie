package opencode

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestSessionEnvReachesEveryLaunch asserts that SORTIE_ISSUE_ID from the
// session environment reaches every process the session launches: the
// version query at session start, each turn's run, and the auxiliary
// subcommands that export or delete a session.
func TestSessionEnvReachesEveryLaunch(t *testing.T) {
	// Not parallel: t.Setenv carries the stale inherited value.
	t.Setenv("SORTIE_ISSUE_ID", "stale")

	runTurn := func(ctx context.Context, adapter domain.AgentAdapter, session domain.Session) {
		_, _ = adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "probe", OnEvent: func(domain.AgentEvent) {}})
	}
	tests := []struct {
		name   string
		launch func(ctx context.Context, adapter domain.AgentAdapter, session domain.Session)
		match  func(agenttest.EnvProbeLaunch) bool
	}{
		{name: "version query", launch: runTurn, match: func(l agenttest.EnvProbeLaunch) bool { return slices.Contains(l.Args, "--version") }},
		{name: "turn", launch: runTurn, match: func(l agenttest.EnvProbeLaunch) bool { return slices.Contains(l.Args, "run") }},
		{
			name: "auxiliary subcommand",
			launch: func(ctx context.Context, _ domain.AgentAdapter, session domain.Session) {
				cmd, err := auxiliaryCommand(ctx, session.Internal.(*sessionState), exportArgs("ses_probe"))
				if err == nil {
					_ = cmd.Run()
				}
			},
			match: func(l agenttest.EnvProbeLaunch) bool { return slices.Contains(l.Args, "export") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.jsonl")
			command := agenttest.FakeRuntime(t, dir, "opencode", agenttest.EnvProbeScenario, agenttest.EnvProbe{
				Path: probe, Names: []string{"SORTIE_ISSUE_ID"}, Version: "2.0.0",
			})
			adapter, err := NewOpenCodeAdapter()
			if err != nil {
				t.Fatalf("NewOpenCodeAdapter() error = %v", err)
			}
			session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig:   domain.AgentConfig{Command: command},
				SessionEnv:    []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "42"}},
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				tt.launch(ctx, adapter, session)
			}()

			launch := agenttest.AwaitEnvProbe(t, probe, tt.match)
			if got := launch.Env["SORTIE_ISSUE_ID"]; got != "42" {
				t.Errorf("launched %v with SORTIE_ISSUE_ID = %q, want %q", launch.Args, got, "42")
			}
		})
	}
}
