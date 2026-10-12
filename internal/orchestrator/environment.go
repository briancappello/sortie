package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

// EnvironmentFunc returns the execution environment for an environment
// configuration.
type EnvironmentFunc func(cfg config.EnvironmentConfig) (environment.Environment, error)

// environmentResolver builds the execution environment the current
// configuration selects through [environment.New], and reuses it while
// the environment block is unchanged, so every worker and cleanup under
// one configuration shares one instance. Safe for concurrent use.
type environmentResolver struct {
	stateDir     string
	workflowPath string
	logger       *slog.Logger

	mu  sync.Mutex
	cfg config.EnvironmentConfig
	env environment.Environment
}

// newEnvironmentResolver returns a resolver whose environments keep their
// state next to dbPath and name workflowPath as their workflow.
func newEnvironmentResolver(dbPath, workflowPath string, logger *slog.Logger) *environmentResolver {
	stateDir := ""
	if dbPath != "" {
		stateDir = filepath.Dir(dbPath)
	}
	return &environmentResolver{stateDir: stateDir, workflowPath: workflowPath, logger: logger}
}

// resolve returns the environment cfg selects. A failure is not cached,
// so a later call retries the construction. A nil resolver builds a new
// environment on every call, with no state directory or workflow path.
func (r *environmentResolver) resolve(cfg config.EnvironmentConfig) (environment.Environment, error) {
	if r == nil {
		return environment.New(environment.Params{Config: cfg, Logger: slog.Default()})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.env != nil && reflect.DeepEqual(r.cfg, cfg) {
		return r.env, nil
	}
	env, err := environment.New(environment.Params{
		Config:       cfg,
		StateDir:     r.stateDir,
		WorkflowPath: r.workflowPath,
		Logger:       r.logger,
	})
	if err != nil {
		return nil, err
	}
	r.cfg = cfg
	r.env = env
	return env, nil
}

// environmentOrUnavailable returns the environment cfg selects, or, when
// it cannot be built, an environment whose every operation fails with
// the construction error, so that no workspace code falls back to the
// host. The failure is logged once per call.
func (o *Orchestrator) environmentOrUnavailable(cfg config.EnvironmentConfig) environment.Environment {
	env, err := o.environments.resolve(cfg)
	if err == nil {
		return env
	}
	o.logger.Warn("execution environment unavailable",
		slog.String("environment", cfg.Kind), slog.Any("error", err))
	return unavailableEnvironment{kind: cfg.Kind, err: err}
}

// unavailableEnvironment stands in for an environment that could not be
// built. Every operation fails with the construction error, so the
// workspace code it would have run runs nowhere.
type unavailableEnvironment struct {
	kind string
	err  error
}

var _ environment.Environment = unavailableEnvironment{}

func (u unavailableEnvironment) failure() error {
	return fmt.Errorf("execution environment %q unavailable: %w", u.kind, u.err)
}

func (u unavailableEnvironment) Kind() string { return u.kind }

func (unavailableEnvironment) GuestPath(hostPath string) string { return hostPath }

func (u unavailableEnvironment) Prepare(context.Context, environment.Workspace) (environment.PrepareResult, error) {
	return environment.PrepareResult{}, u.failure()
}

func (u unavailableEnvironment) Command(context.Context, environment.Workspace, environment.CommandSpec) (*exec.Cmd, error) {
	return nil, u.failure()
}

func (u unavailableEnvironment) Release(context.Context, environment.Workspace) error {
	return u.failure()
}

func (u unavailableEnvironment) Remove(context.Context, environment.Workspace) error {
	return u.failure()
}

func (u unavailableEnvironment) Sweep(context.Context, environment.SweepParams) error {
	return u.failure()
}
