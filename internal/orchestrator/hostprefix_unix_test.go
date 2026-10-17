//go:build unix

package orchestrator

import (
	"context"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/prompt"
)

func TestRunWorkerAttempt_HostPrefixWrapsOnlyTheSessionLaunches(t *testing.T) {
	prefix := []string{"host-prefix-marker", "--"}
	tests := []struct {
		name        string
		prefix      []string
		carrier     bool
		wantSession []string
	}{
		{name: "no prefix"},
		{name: "local", prefix: prefix, wantSession: prefix},
		{name: "carrier", prefix: prefix, carrier: true, wantSession: prefix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := defaultWorkerConfig(root)
			cfg.Agent.MaxTurns = 1
			cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
			cfg.Environment = config.EnvironmentConfig{Kind: config.EnvironmentKindLocal, HostPrefix: tt.prefix}
			cfg.Hooks.AfterCreate = "git init -q && git config commit.gpgsign false && git config user.email test@example.com && git config user.name Test && git commit -q --allow-empty -m initial"
			cfg.Hooks.BeforeRun = "echo prepared > before-run.txt"
			cfg.Hooks.AfterRun = "echo finished > after-run.txt"

			recorder := recordingEnvironment(t)
			var env environment.Environment = recorder
			if tt.carrier {
				env = carrierEnvironment{Recorder: recorder}
			}
			adapter := &paramsRecordingAdapter{}
			ec := newExitCapture()
			RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
				TrackerAdapter:         &mockTrackerAdapter{},
				AgentAdapter:           adapter,
				ConfigFunc:             func() config.ServiceConfig { return cfg },
				PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
				OnEvent:                func(_ string, _ domain.AgentEvent) {},
				OnExit:                 ec.onExit,
				Logger:                 discardLogger(),
				EnvironmentFunc: func(config.EnvironmentConfig) (environment.Environment, error) {
					return env, nil
				},
			})
			result := ec.waitResult(t)
			if result.HandoffEvidenceBaseline == nil {
				t.Errorf("HandoffEvidenceBaseline = nil (error %v), want git inspection to have run", result.HandoffEvidenceBaselineError)
			}

			adapter.mu.Lock()
			defer adapter.mu.Unlock()
			var sawCheck, sawWork bool
			for _, p := range adapter.params {
				if p.CredentialVerification {
					sawCheck = true
				} else {
					sawWork = true
				}
				if !slices.Equal(p.HostPrefix, tt.wantSession) {
					t.Errorf("session (credential check %v) HostPrefix = %q, want %q", p.CredentialVerification, p.HostPrefix, tt.wantSession)
				}
				if got := p.Carrier != nil; got != tt.carrier {
					t.Errorf("session (credential check %v) has a carrier = %v, want %v", p.CredentialVerification, got, tt.carrier)
				}
			}
			if !sawCheck || !sawWork {
				t.Errorf("session starts = %d (credential check %v, work %v), want both", len(adapter.params), sawCheck, sawWork)
			}

			cmds := recorder.Commands()
			if got := len(shellScripts(cmds)); got != 3 {
				t.Errorf("hook commands = %d, want after_create, before_run, and after_run", got)
			}
			if subs := gitSubcommands(cmds); !slices.Contains(subs, "rev-parse") {
				t.Errorf("git commands = %q, want the handoff baseline inspection", subs)
			}
			for _, c := range cmds {
				if slices.Contains(c.Spec.Argv, prefix[0]) {
					t.Errorf("workspace command %q runs behind the host prefix, want only the session launch to", c.Spec.Argv)
				}
			}
		})
	}
}
