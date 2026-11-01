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
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/gitsync"
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

// describeEnvironment returns the description of the execution
// environment instance an issue's workspace runs in under cfg: the
// instance of the environment cfg selects for the workspace of the given
// identifier. An environment that cannot be built, or that names no
// instance, describes none.
func (o *Orchestrator) describeEnvironment(cfg config.ServiceConfig) func(identifier string) environment.Instance {
	resolve := o.issueEnvironment(cfg)
	return func(identifier string) environment.Instance {
		env, ws, ok := resolve(identifier)
		if !ok {
			return environment.Instance{}
		}
		return environment.DescribeInstance(env, ws)
	}
}

// describeKeptDirs returns the host paths of the kept directories of an
// issue's workspace under cfg, as [environment.KeptDirs] names them for
// the workspace of the given identifier. An environment that cannot be
// built keeps none.
func (o *Orchestrator) describeKeptDirs(cfg config.ServiceConfig) func(identifier string) []string {
	resolve := o.issueEnvironment(cfg)
	return func(identifier string) []string {
		env, ws, ok := resolve(identifier)
		if !ok {
			return nil
		}
		return environment.KeptDirs(env, ws)
	}
}

// issueEnvironment returns a resolver of the environment cfg selects and
// of the workspace of an issue identifier in it. It reports false when
// the environment cannot be built or the identifier names no workspace.
func (o *Orchestrator) issueEnvironment(cfg config.ServiceConfig) func(identifier string) (environment.Environment, environment.Workspace, bool) {
	envCfg, root := cfg.Environment, cfg.Workspace.Root
	return func(identifier string) (environment.Environment, environment.Workspace, bool) {
		env, err := o.environments.resolve(envCfg)
		if err != nil {
			return nil, environment.Workspace{}, false
		}
		key, err := workspace.SanitizeKey(identifier)
		if err != nil {
			return nil, environment.Workspace{}, false
		}
		hostPath := filepath.Join(root, key)
		return env, environment.Workspace{
			Key: key, HostPath: hostPath, GuestPath: env.GuestPath(hostPath), Identifier: identifier,
		}, true
	}
}

// Repository places the repository an environment block configures in
// a workspace's environment, and takes the issue branch's committed work
// back out of it, without host code ever reading the workspace.
type Repository interface {
	workspace.RepositorySync

	// Export takes the committed issue branch out of ws's environment
	// and pushes its new commits to the upstream. A failure leaves the
	// work in the environment.
	Export(ctx context.Context, env environment.Environment, ws environment.Workspace) (gitsync.ExportResult, error)
}

// RepositoryFunc returns the [Repository] an environment block
// configures, with its state under stateDir. It is called only for a
// block that configures one; see [configuresRepository].
type RepositoryFunc func(cfg config.EnvironmentConfig, stateDir string, logger *slog.Logger) (Repository, error)

// configuresRepository reports whether cfg places a repository in each
// workspace's environment: the smolvm kind with a repository block.
func configuresRepository(cfg config.EnvironmentConfig) bool {
	return normalizeEnvironmentKind(cfg.Kind) == config.EnvironmentKindSmolvm && cfg.Smolvm.Repository != nil
}

// RepositoryProvider returns the SCM provider that opens the pull
// request for each issue branch Sortie pushes under cfg, or "" when cfg
// configures no repository or the repository names no provider.
func RepositoryProvider(cfg config.EnvironmentConfig) string {
	if !configuresRepository(cfg) {
		return ""
	}
	return cfg.Smolvm.Repository.Provider
}

// issueRepository returns cfg with its repository set to the one the
// issue runs in, and that repository's name. A workspace already seeded
// keeps the repository its record names; otherwise the issue's
// repository label names it, and an issue without one runs in the
// default. A name the workflow does not list is an error. cfg is
// returned unchanged when it configures no repository.
func issueRepository(cfg config.EnvironmentConfig, workspacePath string, labels []string) (config.EnvironmentConfig, string, error) {
	if !configuresRepository(cfg) {
		return cfg, "", nil
	}
	if workspacePath != "" {
		rec, ok, err := workspace.ReadRepositoryRecord(workspacePath)
		if err != nil {
			return cfg, "", fmt.Errorf("read the workspace's repository record: %w", err)
		}
		if ok {
			repo, found := cfg.Smolvm.RepositoryNamed(rec.Name)
			if !found {
				return cfg, "", fmt.Errorf("the workspace was seeded from repository %q, which environment.smolvm.repositories no longer lists", rec.Name)
			}
			cfg.Smolvm.Repository = repo
			return cfg, rec.Name, nil
		}
	}
	name := config.IssueRepositoryName(labels)
	if name == "" {
		return cfg, cfg.Smolvm.Repository.Name, nil
	}
	repo, found := cfg.Smolvm.RepositoryNamed(name)
	if !found {
		return cfg, "", fmt.Errorf("the issue names repository %q, which environment.smolvm.repositories does not list", name)
	}
	cfg.Smolvm.Repository = repo
	return cfg, name, nil
}

// PullRequestOpenerFunc returns the pull request opener of the SCM
// provider cfg's repository names, built from the same settings the SCM
// reactions use. It is called only when [RepositoryProvider] is not
// empty.
type PullRequestOpenerFunc func(cfg config.ServiceConfig) (domain.PullRequestOpener, error)

// repositorySync returns the [Repository] cfg configures, with its
// mirror under stateDir, or nil when cfg configures none.
func repositorySync(cfg config.EnvironmentConfig, stateDir string, logger *slog.Logger) (Repository, error) {
	if !configuresRepository(cfg) {
		return nil, nil
	}
	syncer, err := gitsync.New(gitsync.Params{Repository: *cfg.Smolvm.Repository, StateDir: stateDir, Logger: logger})
	if err != nil {
		return nil, err
	}
	return syncer, nil
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

// RepositoriesProvider returns the SCM provider of the first configured
// repository that names one, or "" when cfg configures no repository or
// none names a provider. Validation makes every named provider the same.
func RepositoriesProvider(cfg config.EnvironmentConfig) string {
	if !configuresRepository(cfg) {
		return ""
	}
	for _, repo := range cfg.Smolvm.AllRepositories() {
		if repo.Provider != "" {
			return repo.Provider
		}
	}
	return ""
}
