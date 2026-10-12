// Package local implements the local execution environment kind: every
// command runs as a plain host process in the host workspace directory.
// It registers itself in [environment.Registry] from init.
package local

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

func init() {
	environment.Registry.Register(config.EnvironmentKindLocal, New)
}

// Environment is the local kind. The zero value is ready to use.
type Environment struct{}

var _ environment.Environment = Environment{}

// New returns the local environment. It reads nothing from params.
func New(environment.Params) (environment.Environment, error) {
	return Environment{}, nil
}

// Kind returns [config.EnvironmentKindLocal].
func (Environment) Kind() string { return config.EnvironmentKindLocal }

// GuestPath returns hostPath unchanged: the host path is the path the
// command sees.
func (Environment) GuestPath(hostPath string) string { return hostPath }

// Prepare does nothing and reports CreatedNow false: the host workspace
// directory is the environment, and its creation is reported by the
// workspace layer.
func (Environment) Prepare(context.Context, environment.Workspace) (environment.PrepareResult, error) {
	return environment.PrepareResult{}, nil
}

// Command returns a host command for spec.Argv bound to ctx, with its
// working directory at spec.Dir under ws.HostPath and its environment
// set to spec.Env verbatim, so nil inherits the host process
// environment. The working directory must be a real directory, not a
// symbolic link, when Command is called. spec.Stdin needs no setting
// here: the caller connects standard input.
func (Environment) Command(ctx context.Context, ws environment.Workspace, spec environment.CommandSpec) (*exec.Cmd, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("local environment: empty command")
	}
	if spec.Dir != "" && !filepath.IsLocal(spec.Dir) {
		return nil, fmt.Errorf("local environment: working directory %q is not inside the workspace", spec.Dir)
	}
	dir := filepath.Join(ws.HostPath, spec.Dir)
	if err := workspacekit.VerifyDir(dir); err != nil {
		return nil, fmt.Errorf("local environment: verify working directory: %w", err)
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // G204: the argv is the workflow's own command, run in its workspace by design.
	cmd.Dir = dir
	cmd.Env = spec.Env
	return cmd, nil
}

// Release does nothing.
func (Environment) Release(context.Context, environment.Workspace) error { return nil }

// Remove does nothing: the workspace layer removes the host directory.
func (Environment) Remove(context.Context, environment.Workspace) error { return nil }

// Sweep does nothing: the local kind owns nothing besides the host
// workspace directories.
func (Environment) Sweep(context.Context, environment.SweepParams) error { return nil }
