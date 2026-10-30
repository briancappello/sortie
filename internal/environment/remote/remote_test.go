package remote

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

func TestRemoteRunsNothingOnHost(t *testing.T) {
	env, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: config.EnvironmentKindRemote}})
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind() != config.EnvironmentKindRemote {
		t.Fatalf("kind %q", env.Kind())
	}
	ws := environment.Workspace{Key: "A-1", HostPath: t.TempDir()}
	if _, err := env.Prepare(context.Background(), ws); !errors.Is(err, ErrRunsOnWorkers) {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := env.Command(context.Background(), ws, environment.CommandSpec{Argv: []string{"true"}}); !errors.Is(err, ErrRunsOnWorkers) {
		t.Fatalf("Command: %v", err)
	}
	if err := env.Remove(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
}
