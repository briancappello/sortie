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
	"github.com/sortie-ai/sortie/internal/workspace"
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

// params returns the construction parameters of the environment cfg
// selects. A nil resolver supplies no state directory or workflow path.
func (r *environmentResolver) params(cfg config.EnvironmentConfig) environment.Params {
	if r == nil {
		return environment.Params{Config: cfg, Logger: slog.Default()}
	}
	return environment.Params{Config: cfg, StateDir: r.stateDir, WorkflowPath: r.workflowPath, Logger: r.logger}
}

// resolve returns the environment cfg selects. A failure is not cached,
// so a later call retries the construction. A nil resolver builds a new
// environment on every call, with no state directory or workflow path.
func (r *environmentResolver) resolve(cfg config.EnvironmentConfig) (environment.Environment, error) {
	if r == nil {
		return environment.New(r.params(cfg))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.env != nil && reflect.DeepEqual(r.cfg, cfg) {
		return r.env, nil
	}
	env, err := environment.New(r.params(cfg))
	if err != nil {
		return nil, err
	}
	r.cfg = cfg
	r.env = env
	return env, nil
}

// forKind returns a builder of the environment of a given kind under
// cfg, for removing a workspace that kind created: cfg's own kind
// resolves to the shared instance, and any other kind is built apart
// from it, from cfg's block with only the kind changed.
func (r *environmentResolver) forKind(cfg config.EnvironmentConfig) func(kind string) (environment.Environment, error) {
	return func(kind string) (environment.Environment, error) {
		if normalizeEnvironmentKind(kind) == normalizeEnvironmentKind(cfg.Kind) {
			return r.resolve(cfg)
		}
		other := cfg
		other.Kind = kind
		return environment.New(r.params(other))
	}
}

// sweepUnselected returns the sweep of every kind cfg does not select,
// as [environment.SweepUnselected] runs it.
func (r *environmentResolver) sweepUnselected(cfg config.EnvironmentConfig) func(ctx context.Context, p environment.SweepParams) error {
	return func(ctx context.Context, p environment.SweepParams) error {
		return environment.SweepUnselected(ctx, r.params(cfg), p)
	}
}

// normalizeEnvironmentKind returns kind, or the local kind for "".
func normalizeEnvironmentKind(kind string) string {
	if kind == "" {
		return config.EnvironmentKindLocal
	}
	return kind
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

// releaseEnvironment releases the execution environment of target, the
// last step of a worker. It runs under a context detached from ctx's
// cancellation, since a cancelled worker releases too. A failure is
// logged and leaves the instance as it was until the next release or
// its removal.
func releaseEnvironment(ctx context.Context, target *workspace.Target, logger *slog.Logger) {
	if target == nil || target.Environment == nil {
		return
	}
	if err := target.Environment.Release(context.WithoutCancel(ctx), target.Workspace); err != nil {
		logger.Warn("execution environment release failed",
			slog.String("environment", target.Environment.Kind()),
			slog.String("workspace", target.Workspace.HostPath), slog.Any("error", err))
	}
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
