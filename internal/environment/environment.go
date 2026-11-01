// Package environment defines the execution environment adapter
// family: where the workspace code of an issue runs. Each kind lives in
// its own subpackage and registers a [Constructor] in [Registry] from
// its init function; the orchestrator resolves the configured kind
// through [Registry] and never imports a kind package.
package environment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Workspace identifies one issue's environment.
type Workspace struct {
	// Key is the sanitized workspace key.
	Key string

	// HostPath is the absolute host workspace directory,
	// <workspace.root>/<key>.
	HostPath string

	// GuestPath is the workspace path inside the environment. It equals
	// HostPath for an environment that runs on the host.
	GuestPath string

	// IssueID and Identifier name the issue the workspace belongs to.
	IssueID    string
	Identifier string
}

// CommandSpec describes one command to run in a workspace.
type CommandSpec struct {
	// Argv is the command and its arguments. Argv[0] is resolved inside
	// the environment. It must not be empty.
	Argv []string

	// Dir is the working directory relative to the workspace root; ""
	// selects the root. It must not escape the workspace.
	Dir string

	// Env holds KEY=VALUE entries: the environment the command sees. An
	// environment that runs on the host uses it verbatim, and nil
	// inherits the host process environment. An environment that runs
	// elsewhere decides which entries cross into it.
	Env []string

	// Stdin keeps the command's standard input open.
	Stdin bool
}

// PrepareResult reports what [Environment.Prepare] did.
type PrepareResult struct {
	// CreatedNow reports that this call created the environment, so the
	// after_create hook must run.
	CreatedNow bool

	// ImageDigest is the hex SHA-256 of the image the workspace's
	// instance was created from, as the instance itself records it, so
	// a later rebuild of the image does not change the value for an
	// instance created before. Empty for an environment without images.
	ImageDigest string
}

// Instance describes the instance a workspace runs in. The zero value
// describes none, which is how a workspace of an environment that runs
// on this host is described.
type Instance struct {
	// Kind is the environment kind.
	Kind string

	// Name names the instance, as [Namer] does.
	Name string

	// ImageDigest is [PrepareResult.ImageDigest].
	ImageDigest string
}

// IsZero reports whether i describes no instance.
func (i Instance) IsZero() bool { return i == Instance{} }

// DescribeInstance returns the instance of env that runs ws: its kind and
// name when env is a [Namer], and the zero [Instance] otherwise. Only
// [Environment.Prepare] reports the image digest; the result carries
// none.
func DescribeInstance(env Environment, ws Workspace) Instance {
	namer, ok := env.(Namer)
	if !ok {
		return Instance{}
	}
	return Instance{Kind: env.Kind(), Name: namer.InstanceName(ws)}
}

// SweepParams carries what [Environment.Sweep] needs to tell a leftover
// environment from one in use.
type SweepParams struct {
	// WorkspaceRoot is the host workspace root. An environment whose
	// <WorkspaceRoot>/<key> directory no longer exists is a leftover.
	WorkspaceRoot string

	// Running holds the workspace keys that have a running worker. Their
	// environments are never deleted.
	Running map[string]bool
}

// Environment runs an issue's workspace code. Implementations must be
// safe for concurrent use across different workspaces.
type Environment interface {
	// Kind returns the registered kind name.
	Kind() string

	// GuestPath returns the path inside the environment for a host
	// path in a workspace.
	GuestPath(hostPath string) string

	// Prepare makes the workspace's environment ready to run commands
	// and reports whether it was created by this call.
	Prepare(ctx context.Context, ws Workspace) (PrepareResult, error)

	// Command returns a command that runs spec inside the workspace's
	// environment. The caller starts and waits for it.
	Command(ctx context.Context, ws Workspace, spec CommandSpec) (*exec.Cmd, error)

	// Release is called as the last step of the issue's worker, on
	// every exit. A workspace with no environment instance releases
	// successfully.
	Release(ctx context.Context, ws Workspace) error

	// Remove deletes the workspace's environment. It is called wherever
	// the workspace itself is removed, after the before_remove hook and
	// before the host directory goes. A workspace with no environment
	// instance is removed successfully.
	Remove(ctx context.Context, ws Workspace) error

	// Sweep deletes environments of this kind that no known workspace
	// references.
	Sweep(ctx context.Context, p SweepParams) error
}

// AfterCreateFinisher is implemented by an environment that needs a
// step after the after_create hook succeeded.
type AfterCreateFinisher interface {
	AfterCreate(ctx context.Context, ws Workspace) error
}

// Resumer is implemented by an environment whose instances stop when a
// worker releases them. A command that runs in a workspace outside a
// worker run, such as the before_remove hook, resumes the instance
// first.
type Resumer interface {
	// Resume makes the existing instance of ws ready to run commands.
	// It reports false when ws has no instance, so a command has
	// nowhere to run.
	Resume(ctx context.Context, ws Workspace) (bool, error)
}

// Namer is implemented by an environment whose workspaces run in a
// named instance, such as a virtual machine. The name is recorded with
// the workspace's kind when the workspace is prepared.
type Namer interface {
	// InstanceName returns the name of the instance that runs ws.
	InstanceName(ws Workspace) string
}

// Carrier is implemented by an environment that runs the agent session
// somewhere other than this host. An environment that does not
// implement it runs the agent the way Sortie runs it without one: on
// this host, or over SSH.
type Carrier interface {
	// LaunchArgv returns the host argv, program first, whose process
	// runs launch.Argv in ws inside the environment, as
	// [domain.LaunchCarrier.LaunchArgv] describes. launch.Env holds the
	// session's identity variables; the environment forwards them by the
	// rules of [GuestEnv].
	LaunchArgv(ws Workspace, launch domain.CarrierLaunch) ([]string, error)

	// ConnectionFailed reports whether a process built from LaunchArgv
	// for ws that exited with exitCode failed before the agent command
	// started, as [domain.LaunchCarrier.ConnectionFailed] describes.
	ConnectionFailed(ws Workspace, exitCode int, stderrTail []string) bool
}

// ToolChanneler is implemented by an environment whose instances reach a
// tool server on this host through a channel the environment keeps open,
// as [domain.ToolChannel] describes.
type ToolChanneler interface {
	// ToolChannel returns ws's channel. The directory of its host socket
	// belongs to ws alone, outside the workspace and out of the instance's
	// reach, and is removed with ws's environment.
	ToolChannel(ws Workspace) (domain.ToolChannel, error)
}

// KeptDirLister is implemented by an environment that gives each
// workspace host directories of its own, which it creates before the
// workspace's instance and never removes, so what the instance wrote in
// them outlives the instance and the workspace.
type KeptDirLister interface {
	// KeptDirs returns the host paths of ws's kept directories.
	KeptDirs(ws Workspace) []string
}

// KeptDirs returns the host paths of ws's kept directories when env is
// a [KeptDirLister], and nil otherwise.
func KeptDirs(env Environment, ws Workspace) []string {
	lister, ok := env.(KeptDirLister)
	if !ok {
		return nil
	}
	return lister.KeptDirs(ws)
}

// TransferDirNamer is implemented by an environment that names the
// directory inside it where files Sortie passes into and out of a
// workspace's instance are staged, because its commands cannot write the
// directory a caller would use by default.
type TransferDirNamer interface {
	// GuestTransferDir returns the absolute guest directory, outside the
	// workspace and every directory the host shares, that the
	// environment's commands can create and write. "" keeps the caller's
	// default.
	GuestTransferDir() string
}

// GuestTransferDir returns the transfer directory env names when it is a
// [TransferDirNamer] that names one, and fallback otherwise.
func GuestTransferDir(env Environment, fallback string) string {
	if namer, ok := env.(TransferDirNamer); ok {
		if dir := namer.GuestTransferDir(); dir != "" {
			return dir
		}
	}
	return fallback
}

// EnvForwarder is implemented by an environment that runs elsewhere and
// makes chosen host variables available to the commands it runs. Any
// other host variable never reaches them.
type EnvForwarder interface {
	// ForwardsEnv reports whether the configuration makes the host
	// variable name available in the environment, by any means: its
	// value, or a placeholder that stands for it.
	ForwardsEnv(name string) bool
}

// RunsElsewhere reports whether env runs workspace code somewhere other
// than this host, which an environment declares by implementing
// [Carrier]. A command such an environment builds does not see this
// host's files, and a host variable reaches it only as the environment
// forwards it. A nil env runs on this host.
func RunsElsewhere(env Environment) bool {
	_, ok := env.(Carrier)
	return ok
}

// LaunchCarrier returns the agent launch carrier for ws when env
// implements [Carrier], and nil otherwise, so a nil result launches the
// agent as Sortie launches it without an environment.
func LaunchCarrier(env Environment, ws Workspace) domain.LaunchCarrier {
	carrier, ok := env.(Carrier)
	if !ok {
		return nil
	}
	return boundCarrier{carrier: carrier, ws: ws}
}

// boundCarrier is a [Carrier] bound to one workspace.
type boundCarrier struct {
	carrier Carrier
	ws      Workspace
}

func (b boundCarrier) LaunchArgv(launch domain.CarrierLaunch) ([]string, error) {
	return b.carrier.LaunchArgv(b.ws, launch)
}

func (b boundCarrier) Workspace() string {
	return b.ws.GuestPath
}

func (b boundCarrier) ToolChannel() (domain.ToolChannel, bool) {
	channeler, ok := b.carrier.(ToolChanneler)
	if !ok {
		return domain.ToolChannel{}, false
	}
	channel, err := channeler.ToolChannel(b.ws)
	if err != nil {
		return domain.ToolChannel{}, false
	}
	return channel, true
}

func (b boundCarrier) ConnectionFailed(exitCode int, stderrTail []string) bool {
	return b.carrier.ConnectionFailed(b.ws, exitCode, stderrTail)
}

// GuestPathOf returns the path inside the environment for hostPath, a
// path in ws on this host: the same path relative to ws.GuestPath, with
// slash separators. A path outside ws.HostPath is returned unchanged.
func (ws Workspace) GuestPathOf(hostPath string) string {
	if ws.HostPath == "" || ws.GuestPath == "" {
		return hostPath
	}
	rel, err := filepath.Rel(ws.HostPath, hostPath)
	if err != nil {
		return hostPath
	}
	if rel == "." {
		return ws.GuestPath
	}
	if !filepath.IsLocal(rel) {
		return hostPath
	}
	return path.Join(ws.GuestPath, filepath.ToSlash(rel))
}

// Session variables an environment that runs elsewhere treats apart
// from the rest.
const (
	// SessionTokenEnv and APIURLEnv name the host API a session reaches.
	// Neither crosses into such an environment: it cannot reach the host
	// loopback API.
	SessionTokenEnv = "SORTIE_SESSION_TOKEN"
	APIURLEnv       = "SORTIE_API_URL"

	// WorkspaceEnv names the workspace path. It is rewritten to the path
	// inside the environment.
	WorkspaceEnv = "SORTIE_WORKSPACE"

	// KindEnv names the kind of the environment a command runs in.
	KindEnv = "SORTIE_ENVIRONMENT"

	sortieEnvPrefix = "SORTIE_"
)

// GuestEnv returns the KEY=VALUE entries of env that cross into an
// environment of kind that runs elsewhere, whose workspace path is
// guestWorkspace: every SORTIE_* entry except SORTIE_SESSION_TOKEN,
// SORTIE_API_URL, SORTIE_WORKSPACE, and SORTIE_ENVIRONMENT, in order,
// followed by SORTIE_WORKSPACE=guestWorkspace and
// SORTIE_ENVIRONMENT=kind. No other entry crosses. A name env repeats
// keeps its first entry.
func GuestEnv(env []string, guestWorkspace, kind string) []string {
	out := make([]string, 0, len(env)+2)
	seen := make(map[string]bool, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, sortieEnvPrefix) || seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case SessionTokenEnv, APIURLEnv, WorkspaceEnv, KindEnv:
			continue
		}
		out = append(out, entry)
	}
	return append(out, WorkspaceEnv+"="+guestWorkspace, KindEnv+"="+kind)
}

// Params carries what a [Constructor] needs to build an [Environment].
type Params struct {
	// Config is the parsed environment block.
	Config config.EnvironmentConfig

	// StateDir is the directory Sortie keeps its own state in, next to
	// the database.
	StateDir string

	// WorkflowPath is the path of the workflow file.
	WorkflowPath string

	// Logger receives the environment's diagnostics. Never nil when
	// the orchestrator constructs an environment.
	Logger *slog.Logger
}

// Constructor builds an [Environment] of one kind.
type Constructor func(params Params) (Environment, error)

// Meta holds optional properties a kind declares at registration.
// The zero value declares nothing.
type Meta struct {
	// CheckHost runs once at startup and on each reload that selects
	// the kind, to check that the host can run it. Nil means there is
	// nothing to check.
	CheckHost func(cfg config.EnvironmentConfig) error

	// SweepUnselected deletes the kind's leftover instances, as
	// [Environment.Sweep] does, while the configuration selects another
	// kind, so instances left from when a workflow selected the kind
	// are still swept after it stops selecting it. params describes the
	// kind's environment as [New] receives it. It must return without
	// running anything when the host holds no sign of an instance. Nil
	// means the kind leaves nothing to sweep once unselected.
	SweepUnselected func(ctx context.Context, params Params, p SweepParams) error
}

// Registry is the environment kind registry. Kind packages register
// themselves from their init functions; the orchestrator resolves the
// configured kind at runtime.
var Registry = registry.NewRegistry[Constructor, Meta]("environment")

// CheckHost runs the host check of the kind cfg selects, resolved
// through [Registry] as [New] resolves it. A kind that declares no check
// passes; an unregistered kind is an error.
func CheckHost(cfg config.EnvironmentConfig) error {
	kind := cfg.Kind
	if kind == "" {
		kind = config.EnvironmentKindLocal
	}
	if _, err := Registry.Get(kind); err != nil {
		return err
	}
	meta, _ := Registry.Meta(kind)
	if meta.CheckHost == nil {
		return nil
	}
	return meta.CheckHost(cfg)
}

// SweepUnselected runs [Meta.SweepUnselected] of every registered kind
// other than the one params.Config selects, with params.Config's kind
// changed to that kind, and joins their errors. An empty kind selects
// [config.EnvironmentKindLocal].
func SweepUnselected(ctx context.Context, params Params, p SweepParams) error {
	selected := params.Config.Kind
	if selected == "" {
		selected = config.EnvironmentKindLocal
	}
	var errs []error
	for _, kind := range Registry.Kinds() {
		if kind == selected {
			continue
		}
		meta, _ := Registry.Meta(kind)
		if meta.SweepUnselected == nil {
			continue
		}
		kindParams := params
		kindParams.Config.Kind = kind
		if err := meta.SweepUnselected(ctx, kindParams, p); err != nil {
			errs = append(errs, fmt.Errorf("sweep environment %q: %w", kind, err))
		}
	}
	return errors.Join(errs...)
}

// New builds the environment params.Config selects by resolving its kind
// through [Registry]. An empty kind selects [config.EnvironmentKindLocal],
// the kind of a configuration built without the environment block. An
// unregistered kind is an error: there is no fallback to another kind.
func New(params Params) (Environment, error) {
	kind := params.Config.Kind
	if kind == "" {
		kind = config.EnvironmentKindLocal
	}
	constructor, err := Registry.Get(kind)
	if err != nil {
		return nil, err
	}
	env, err := constructor(params)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", kind, err)
	}
	return env, nil
}
