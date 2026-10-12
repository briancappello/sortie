// Package smolvm registers the smolvm execution environment kind, which
// runs each issue workspace in its own smolvm microVM. It registers
// itself in [environment.Registry] from init.
//
// The package drives smolvm through its command line only, with [CLI];
// it is the one place that knows smolvm's flags, JSON output, and exit
// codes. The kind's host check, [CheckHost], refuses a smolvm release
// outside [SupportedVersion].
//
// This version registers the kind and its type only. Preparing a
// workspace and building a command both fail with [ErrNotImplemented],
// so a workflow that selects the kind fails workspace preparation and
// never runs an issue's hooks or agent on the host instead.
package smolvm

import (
	"context"
	"errors"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
)

// ErrNotImplemented reports that the smolvm kind cannot yet prepare a
// VM or run a command in one.
var ErrNotImplemented = errors.New("smolvm environment: VM preparation and command execution are not implemented in this build")

func init() {
	environment.Registry.RegisterWithMeta(config.EnvironmentKindSmolvm, New, environment.Meta{CheckHost: CheckHost})
}

// Environment is the smolvm kind.
type Environment struct{}

var (
	_ environment.Environment = (*Environment)(nil)
	_ environment.Carrier     = (*Environment)(nil)
)

// New returns the smolvm environment. It reads nothing from params in
// this build.
func New(environment.Params) (environment.Environment, error) {
	return &Environment{}, nil
}

// Kind returns [config.EnvironmentKindSmolvm].
func (*Environment) Kind() string { return config.EnvironmentKindSmolvm }

// GuestPath returns hostPath unchanged. No command runs in a VM in this
// build, so no host path is ever handed to one.
func (*Environment) GuestPath(hostPath string) string { return hostPath }

// Prepare fails with [ErrNotImplemented].
func (*Environment) Prepare(context.Context, environment.Workspace) (environment.PrepareResult, error) {
	return environment.PrepareResult{}, ErrNotImplemented
}

// Command fails with [ErrNotImplemented].
func (*Environment) Command(context.Context, environment.Workspace, environment.CommandSpec) (*exec.Cmd, error) {
	return nil, ErrNotImplemented
}

// LaunchArgv fails with [ErrNotImplemented]. The kind implements
// [environment.Carrier] so that an agent session it is asked to carry
// fails instead of starting on the host.
func (*Environment) LaunchArgv(environment.Workspace, domain.CarrierLaunch) ([]string, error) {
	return nil, ErrNotImplemented
}

// ConnectionFailed reports false: [Environment.LaunchArgv] never builds
// a launch in this build.
func (*Environment) ConnectionFailed(int, []string) bool { return false }

// Release does nothing: [Environment.Prepare] never creates a VM.
func (*Environment) Release(context.Context, environment.Workspace) error { return nil }

// Remove does nothing: [Environment.Prepare] never creates a VM.
func (*Environment) Remove(context.Context, environment.Workspace) error { return nil }

// Sweep does nothing: [Environment.Prepare] never creates a VM.
func (*Environment) Sweep(context.Context, environment.SweepParams) error { return nil }
