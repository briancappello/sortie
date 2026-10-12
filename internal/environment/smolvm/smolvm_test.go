package smolvm

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/environment"
)

func TestUnimplementedOperationsFail(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ws := environment.Workspace{Key: "VM-1", HostPath: t.TempDir()}
	env, err := New(environment.Params{})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if env.Kind() != "smolvm" {
		t.Errorf("Kind() = %q, want %q", env.Kind(), "smolvm")
	}

	res, err := env.Prepare(ctx, ws)
	if !errors.Is(err, ErrNotImplemented) || res.CreatedNow {
		t.Errorf("Prepare() = %+v, %v, want CreatedNow false and ErrNotImplemented", res, err)
	}
	cmd, err := env.Command(ctx, ws, environment.CommandSpec{Argv: []string{"uname", "-n"}})
	if !errors.Is(err, ErrNotImplemented) || cmd != nil {
		t.Errorf("Command() = %v, %v, want nil and ErrNotImplemented: no command may run on the host instead", cmd, err)
	}
	for name, op := range map[string]func() error{
		"Release": func() error { return env.Release(ctx, ws) },
		"Remove":  func() error { return env.Remove(ctx, ws) },
		"Sweep":   func() error { return env.Sweep(ctx, environment.SweepParams{}) },
	} {
		if err := op(); err != nil {
			t.Errorf("%s() = %v, want nil: no VM was ever created", name, err)
		}
	}
}
