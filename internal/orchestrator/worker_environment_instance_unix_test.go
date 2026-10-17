//go:build unix

package orchestrator

import (
	"context"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/prompt"
)

func TestRunWorkerAttempt_ReportsTheEnvironmentInstance(t *testing.T) {
	root := t.TempDir()
	cfg := defaultWorkerConfig(root)
	cfg.Agent.MaxTurns = 1
	rec := recordingEnvironment(t)
	rec.KindName = "vmkind"
	rec.PrepareResult = environment.PrepareResult{ImageDigest: "0123abcd"}
	env := namingEnvironment{rec}

	ec := newExitCapture()
	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
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

	key := workerTestIssue().Identifier
	want := environment.Instance{Kind: "vmkind", Name: "vm-" + key, ImageDigest: "0123abcd"}
	if result.EnvironmentInstance != want {
		t.Errorf("WorkerResult.EnvironmentInstance = %+v, want %+v", result.EnvironmentInstance, want)
	}
}
