package clientprotocol

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestSessionEnvReachesEveryLaunch asserts that SORTIE_ISSUE_ID from the
// session environment reaches the agent process, the only process a
// session launches, for a work session and a credential verification.
//
// This package names no SORTIE_ variable in an environment position, so
// the override of an inherited value is proven by the shared launch
// target's own tests.
func TestSessionEnvReachesEveryLaunch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		verification bool
	}{
		{name: "agent for a work session"},
		{name: "agent for a credential verification", verification: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			probe := filepath.Join(dir, "probe.jsonl")
			command := agenttest.FakeRuntime(t, dir, "acp-agent", agenttest.EnvProbeScenario, agenttest.EnvProbe{
				Path: probe, Names: []string{"SORTIE_ISSUE_ID"},
			})
			adapter, err := NewClientProtocolAdapter()
			if err != nil {
				t.Fatalf("NewClientProtocolAdapter() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				session, err := adapter.StartSession(ctx, domain.StartSessionParams{
					WorkspacePath:          t.TempDir(),
					AgentConfig:            domain.AgentConfig{Kind: "agent-client-protocol", Command: command},
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
