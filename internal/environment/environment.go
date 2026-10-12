// Package environment defines the execution environment adapter
// family: where the workspace code of an issue runs. Each kind lives in
// its own subpackage and registers a [Constructor] in [Registry] from
// its init function; the orchestrator resolves the configured kind
// through [Registry] and never imports a kind package.
package environment

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/config"
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

	// Release is called when the issue's worker has exited.
	Release(ctx context.Context, ws Workspace) error

	// Remove deletes the workspace's environment. It is called wherever
	// the workspace itself is removed.
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

// Namer is implemented by an environment whose workspaces run in a
// named instance, such as a virtual machine. The name is recorded with
// the workspace's kind when the workspace is prepared.
type Namer interface {
	// InstanceName returns the name of the instance that runs ws.
	InstanceName(ws Workspace) string
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
}

// Registry is the environment kind registry. Kind packages register
// themselves from their init functions; the orchestrator resolves the
// configured kind at runtime.
var Registry = registry.NewRegistry[Constructor, Meta]("environment")

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
