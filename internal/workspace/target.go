package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// Target is where the commands Sortie runs against one workspace run:
// the execution environment and the workspace it serves. Every hook,
// verification command, triage command, and git inspection builds its
// process through [Target.Command].
type Target struct {
	// Environment builds each command. Nil runs commands as plain host
	// processes in Workspace.HostPath, which is how the local kind runs
	// them too; callers that hold a resolved environment always set it,
	// so a nil Environment never stands in for another kind.
	Environment environment.Environment

	// Workspace identifies the workspace to Environment. With a nil
	// Environment only HostPath is read.
	Workspace environment.Workspace
}

// HostTarget returns the [Target] that runs commands as host processes
// in dir.
func HostTarget(dir string) Target {
	return Target{Workspace: environment.Workspace{HostPath: dir, GuestPath: dir}}
}

// NewTarget returns the [Target] for the workspace at hostPath, whose
// sanitized key is key, run by env. A nil env yields a host target.
func NewTarget(env environment.Environment, key, hostPath, issueID, identifier string) Target {
	guestPath := hostPath
	if env != nil {
		guestPath = env.GuestPath(hostPath)
	}
	return Target{
		Environment: env,
		Workspace: environment.Workspace{
			Key:        key,
			HostPath:   hostPath,
			GuestPath:  guestPath,
			IssueID:    issueID,
			Identifier: identifier,
		},
	}
}

// GuestPath returns the path inside the target's environment for a host
// path in the workspace: hostPath itself without an environment.
func (t Target) GuestPath(hostPath string) string {
	if t.Environment == nil {
		return hostPath
	}
	return t.Environment.GuestPath(hostPath)
}

// Command returns a command for spec bound to ctx. With an Environment
// it is the environment's command. Without one it is a host process
// whose working directory is spec.Dir under Workspace.HostPath, verified
// as a real directory, and whose environment is spec.Env verbatim, so
// nil inherits the host process environment. The caller starts and
// waits for the command.
func (t Target) Command(ctx context.Context, spec environment.CommandSpec) (*exec.Cmd, error) {
	if t.Environment != nil {
		return t.Environment.Command(ctx, t.Workspace, spec)
	}
	if len(spec.Argv) == 0 {
		return nil, errors.New("empty command")
	}
	dir := t.Workspace.HostPath
	if spec.Dir != "" {
		if !filepath.IsLocal(spec.Dir) {
			return nil, fmt.Errorf("working directory %q is not inside the workspace", spec.Dir)
		}
		dir = filepath.Join(dir, spec.Dir)
	}
	if err := workspacekit.VerifyDir(dir); err != nil {
		return nil, fmt.Errorf("verify working directory: %w", err)
	}
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // G204: the argv is Sortie's own git invocation or the workflow's configured command, run in its workspace by design
	cmd.Dir = dir
	cmd.Env = spec.Env
	return cmd, nil
}

// GitCommand returns a git command for args that runs in the target's
// workspace root, once the host workspace directory verifies as a real,
// unswapped directory. env is the command's environment as in
// [environment.CommandSpec]; nil inherits the host process environment.
// On Windows it disables the filesystem monitor: a monitor daemon a
// query starts stays in the launch's process containment and ends at the
// reap, so it would answer no later query, and every later query would
// start another one for nothing. On Linux and macOS the monitor detaches
// from the launch and keeps running, so the operator's own setting is
// left alone there.
func (t Target) GitCommand(ctx context.Context, env []string, args ...string) (*exec.Cmd, error) {
	return t.gitCommand(ctx, env, false, args...)
}

// gitCommand is [Target.GitCommand] with standard input kept open when
// stdin is true, as [environment.CommandSpec.Stdin] describes; the
// caller connects it.
func (t Target) gitCommand(ctx context.Context, env []string, stdin bool, args ...string) (*exec.Cmd, error) {
	if err := workspacekit.VerifyDir(t.Workspace.HostPath); err != nil {
		return nil, fmt.Errorf("verify git working directory: %w", err)
	}

	argv := make([]string, 0, len(args)+3)
	argv = append(argv, "git")
	if runtime.GOOS == "windows" {
		argv = append(argv, "-c", "core.fsmonitor=false")
	}
	argv = append(argv, args...)
	return t.Command(ctx, environment.CommandSpec{Argv: argv, Env: env, Stdin: stdin})
}

// relativeDir returns dir relative to the target's workspace root, as
// [environment.CommandSpec.Dir] takes it: "" for the root itself, and an
// error for a directory outside the workspace.
func (t Target) relativeDir(dir string) (string, error) {
	rel, err := filepath.Rel(t.Workspace.HostPath, dir)
	if err != nil {
		return "", fmt.Errorf("directory %q is not inside workspace %q: %w", dir, t.Workspace.HostPath, err)
	}
	if rel == "." {
		return "", nil
	}
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("directory %q is not inside workspace %q", dir, t.Workspace.HostPath)
	}
	return rel, nil
}
