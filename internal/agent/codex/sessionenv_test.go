package codex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestSessionEnvReachesEveryLaunch asserts that SORTIE_ISSUE_ID from the
// session environment reaches the app-server process, the only process
// a session launches, for a work session and a credential verification.
func TestSessionEnvReachesEveryLaunch(t *testing.T) {
	// Not parallel: t.Setenv carries the stale inherited value.
	t.Setenv("SORTIE_ISSUE_ID", "stale")

	tests := []struct {
		name         string
		verification bool
	}{
		{name: "app-server for a work session"},
		{name: "app-server for a credential verification", verification: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.jsonl")
			command := agenttest.FakeRuntime(t, dir, "codex", agenttest.EnvProbeScenario, agenttest.EnvProbe{
				Path: probe, Names: []string{"SORTIE_ISSUE_ID"},
			})
			adapter, err := NewCodexAdapter()
			if err != nil {
				t.Fatalf("NewCodexAdapter() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				session, err := adapter.StartSession(ctx, domain.StartSessionParams{
					WorkspacePath:          t.TempDir(),
					AgentConfig:            domain.AgentConfig{Command: command},
					CredentialVerification: tt.verification,
					SessionEnv:             []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "42"}},
				})
				if err == nil {
					_ = adapter.StopSession(context.Background(), session)
				}
			}()

			launch := agenttest.AwaitEnvProbe(t, probe, func(agenttest.EnvProbeLaunch) bool { return true })
			if got := launch.Env["SORTIE_ISSUE_ID"]; got != "42" {
				t.Errorf("launched SORTIE_ISSUE_ID = %q, want %q", got, "42")
			}
		})
	}
}
