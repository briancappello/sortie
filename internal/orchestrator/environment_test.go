package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

// countingRegistration registers the counting kind once per test binary,
// so the test also passes when run repeatedly in one process.
var (
	countingRegistration sync.Once
	countingBuilds       atomic.Int64
)

func TestEnvironmentResolver_ReusesEnvironmentWhileConfigUnchanged(t *testing.T) {

	const kind = "orchestrator-test-counting"
	countingRegistration.Do(func() {
		environment.Registry.Register(kind, func(environment.Params) (environment.Environment, error) {
			countingBuilds.Add(1)
			return unavailableEnvironment{kind: kind, err: errors.New("unused")}, nil
		})
	})
	start := countingBuilds.Load()
	builds := func() int64 { return countingBuilds.Load() - start }

	r := newEnvironmentResolver("/state/sortie.db", "/wf/WORKFLOW.md", discardLogger())
	cfg := config.EnvironmentConfig{Kind: kind, HostPrefix: []string{"a"}}
	if _, err := r.resolve(cfg); err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if _, err := r.resolve(config.EnvironmentConfig{Kind: kind, HostPrefix: []string{"a"}}); err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if builds() != 1 {
		t.Errorf("constructor calls = %d, want 1 for an unchanged environment block", builds())
	}
	if _, err := r.resolve(config.EnvironmentConfig{Kind: kind, HostPrefix: []string{"b"}}); err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if builds() != 2 {
		t.Errorf("constructor calls = %d, want 2 after the environment block changed", builds())
	}
}

func TestEnvironmentResolver_EmptyKindResolvesLocal(t *testing.T) {
	t.Parallel()

	env, err := newEnvironmentResolver("", "", discardLogger()).resolve(config.EnvironmentConfig{})
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if env.Kind() != config.EnvironmentKindLocal {
		t.Errorf("Kind() = %q, want %q", env.Kind(), config.EnvironmentKindLocal)
	}
}

func TestEnvironmentOrUnavailable_FailsEveryOperation(t *testing.T) {
	t.Parallel()

	o := &Orchestrator{logger: discardLogger(), environments: newEnvironmentResolver("", "", discardLogger())}
	env := o.environmentOrUnavailable(config.EnvironmentConfig{Kind: "no-such-kind"})

	if env.Kind() != "no-such-kind" {
		t.Errorf("Kind() = %q, want no-such-kind", env.Kind())
	}
	ctx := context.Background()
	ws := environment.Workspace{HostPath: t.TempDir()}
	if cmd, err := env.Command(ctx, ws, environment.CommandSpec{Argv: []string{"true"}}); err == nil || cmd != nil {
		t.Errorf("Command() = %v, %v; want no command and an error", cmd, err)
	}
	if _, err := env.Prepare(ctx, ws); err == nil {
		t.Error("Prepare() error = nil, want an error")
	}
	if err := env.Remove(ctx, ws); err == nil {
		t.Error("Remove() error = nil, want an error")
	}
}
