// Package remote implements the remote execution environment kind. Its
// runs go to the worker matcher and run on workers that dial Sortie, so
// this environment never runs a command on Sortie's host. It registers
// itself in [environment.Registry] from init.
package remote

import (
	"context"
	"errors"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

// ErrRunsOnWorkers is returned for any request to run a command: under
// the remote kind, workspace code runs only on workers.
var ErrRunsOnWorkers = errors.New("remote environment: workspace code runs on workers, not on Sortie's host")

func init() {
	environment.Registry.Register(config.EnvironmentKindRemote, New)
}

// Environment is the remote kind. The zero value is ready to use.
type Environment struct{}

var _ environment.Environment = Environment{}

// New returns the remote environment. It reads nothing from params.
func New(environment.Params) (environment.Environment, error) {
	return Environment{}, nil
}

// Kind returns [config.EnvironmentKindRemote].
func (Environment) Kind() string { return config.EnvironmentKindRemote }

// GuestPath returns hostPath unchanged.
func (Environment) GuestPath(hostPath string) string { return hostPath }

// Prepare refuses: nothing of an issue is prepared on Sortie's host.
func (Environment) Prepare(context.Context, environment.Workspace) (environment.PrepareResult, error) {
	return environment.PrepareResult{}, ErrRunsOnWorkers
}

// Command refuses: nothing of an issue runs on Sortie's host.
func (Environment) Command(context.Context, environment.Workspace, environment.CommandSpec) (*exec.Cmd, error) {
	return nil, ErrRunsOnWorkers
}

// Release does nothing: the worker owns the machine.
func (Environment) Release(context.Context, environment.Workspace) error { return nil }

// Remove does nothing: the worker owns the machine.
func (Environment) Remove(context.Context, environment.Workspace) error { return nil }

// Sweep does nothing: no instance of this kind lives on Sortie's host.
func (Environment) Sweep(context.Context, environment.SweepParams) error { return nil }
