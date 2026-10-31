package smolvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// Command returns the host process that runs spec.Argv in the
// workspace's VM: smolvm's exec of it in the guest workspace, joined
// with spec.Dir, which must not leave it. spec.Stdin keeps the guest
// command's standard input open as a byte-exact pipe; without it the
// guest command reads an empty standard input, and its output streams
// as it is written either way. Ending the host process ends the guest
// command's process group.
//
// The guest command's environment is built by [environment.GuestEnv]
// from spec.Env, carried in the argv, plus every pass_env variable
// set on the host, which smolvm resolves from its own environment so
// the value is never in the argv. A pass_env variable that is not set
// on the host is left out: there is no value to copy, and smolvm would
// refuse to run the command. The session token and the API URL never
// cross, even when pass_env lists them. The host process itself runs
// with Sortie's environment, which the smolvm client needs.
//
// The guest command's exit status is the host process's. A smolvm
// failure before the guest command started, such as a VM that is not
// running, exits with status 1 and smolvm's JSON failure report as the
// last line of standard error; see [Environment.ConnectionFailed].
func (e *Environment) Command(ctx context.Context, ws environment.Workspace, spec environment.CommandSpec) (*exec.Cmd, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("smolvm environment: empty command")
	}
	workdir, err := guestDir(spec.Dir)
	if err != nil {
		return nil, err
	}
	name, err := execVMName(ws)
	if err != nil {
		return nil, err
	}
	env := environment.GuestEnv(spec.Env, GuestWorkspace, config.EnvironmentKindSmolvm)
	opts := ExecOptions{
		Workdir:     workdir,
		Env:         env,
		SecretEnv:   e.passEnv(env),
		Interactive: spec.Stdin,
		Stream:      !spec.Stdin,
	}
	cmd := exec.CommandContext(ctx, e.cli.Binary(), ExecArgs(name, opts, spec.Argv)...) //nolint:gosec // G204: the operator configures the binary; the guest argv is the workflow's own command, run in its VM by design.
	return cmd, nil
}

// LaunchArgv returns the host argv that runs launch.Argv in the
// workspace's VM, with standard input and output connected as a
// byte-exact pipe, in the guest workspace. The guest command's
// environment is [environment.GuestEnv] of launch.Env, then
// launch.AgentEnv unchanged, then the pass_env variables as
// [Environment.Command] passes them, and each launch.SecretEnv name the
// same way: smolvm resolves its value from the host process's environment,
// so it is never in the argv. A name in launch.Unset is removed
// in the guest with env -u before the agent starts, so the agent runs
// without it even where the VM's image sets it.
func (e *Environment) LaunchArgv(ws environment.Workspace, launch domain.CarrierLaunch) ([]string, error) {
	if len(launch.Argv) == 0 {
		return nil, errors.New("smolvm environment: empty agent command")
	}
	name, err := execVMName(ws)
	if err != nil {
		return nil, err
	}
	env := environment.GuestEnv(launch.Env, GuestWorkspace, config.EnvironmentKindSmolvm)
	env = append(env, launch.AgentEnv...)
	argv := launch.Argv
	if len(launch.Unset) > 0 {
		argv = make([]string, 0, 2+2*len(launch.Unset)+len(launch.Argv))
		argv = append(argv, "env")
		for _, unset := range launch.Unset {
			if unset == "" || strings.ContainsAny(unset, "=\x00") {
				return nil, fmt.Errorf("smolvm environment: %q is not a variable name to unset", unset)
			}
			argv = append(argv, "-u", unset)
		}
		argv = append(argv, "--")
		argv = append(argv, launch.Argv...)
	}
	secrets := e.passEnv(env)
	for _, secret := range launch.SecretEnv {
		if !validEnvName(secret) {
			return nil, fmt.Errorf("smolvm environment: %q is not a variable name to pass", secret)
		}
		secrets = append(secrets, secret+"="+secret)
	}
	opts := ExecOptions{
		Workdir:     GuestWorkspace,
		Env:         env,
		SecretEnv:   secrets,
		Interactive: true,
	}
	return append([]string{e.cli.Binary()}, ExecArgs(name, opts, argv)...), nil
}

// ConnectionFailed reports whether a host process built by
// [Environment.LaunchArgv] or [Environment.Command] for ws that exited
// with exitCode failed before the guest command started, or lost the VM
// while it ran. That is exit status 1 with the last non-blank line of
// stderrTail being smolvm's own JSON failure report with a code of
// [connectionFailureCodes], such as a VM that does not exist or is not
// running, a closed connection, or a secret that could not be resolved;
// or exit status 2 with smolvm's usage error for its exec command,
// which it reports only as text. A guest command's own failure,
// including one that cannot be started in the guest such as a missing
// program, is not a connection failure: smolvm passes the guest's
// standard error through unchanged and adds no report. The guest's
// standard error reaches the same stream, so only an exact report, as
// the last line, counts.
func (*Environment) ConnectionFailed(ws environment.Workspace, exitCode int, stderrTail []string) bool {
	if ws.Key == "" {
		return false
	}
	last := lastLine(stderrTail)
	if last == "" {
		return false
	}
	switch exitCode {
	case 1:
		code, _, ok := parseErrorReport(last)
		return ok && slices.Contains(connectionFailureCodes, code)
	case 2:
		return last == usageErrorLast && slices.ContainsFunc(stderrTail, func(line string) bool {
			return strings.HasPrefix(strings.TrimSpace(line), usageErrorUsagePrefix)
		})
	}
	return false
}

// connectionFailureCodes are the failure codes smolvm's exec reports for
// a failure of its own, before the guest command started or under it.
// smolvm 1.32.4 reports a missing VM as [codeNotFound], a stopped VM
// and a connection lost mid-command as [codeSmolvm], and an unset
// --secret-env variable as [codeConfig]. COMMAND_FAILED is left out: it
// reports a command's failure, not smolvm's.
var connectionFailureCodes = []string{
	codeNotFound, codeConflict, codeInterrupted, codeConfig, codeInvalidState,
	codeStorage, codeMount, codeKVMUnavailable, codeHypervisorUnavailable, codeSmolvm,
}

// smolvm's usage error for its exec command, which it writes as text
// even with --error-format json, ends with these lines, with exit
// status 2.
const (
	usageErrorUsagePrefix = "Usage: smolvm machine exec "
	usageErrorLast        = "For more information, try '--help'."
)

// guestDir returns the guest working directory for dir, a directory
// relative to the workspace root.
func guestDir(dir string) (string, error) {
	if dir == "" {
		return GuestWorkspace, nil
	}
	if !filepath.IsLocal(dir) {
		return "", fmt.Errorf("smolvm environment: working directory %q is not inside the workspace", dir)
	}
	return path.Join(GuestWorkspace, filepath.ToSlash(dir)), nil
}

// execVMName returns the name of ws's VM for a command run in it.
func execVMName(ws environment.Workspace) (string, error) {
	if ws.Key == "" {
		return "", errors.New("smolvm environment: the workspace has no key")
	}
	return VMName(ws.Key), nil
}

// passEnv returns the --secret-env references of the pass_env
// variables set on the host, in configuration order, without the
// session token, the API URL, a name repeated, or a name already set
// in env, whose value Sortie owns.
func (e *Environment) passEnv(env []string) []string {
	if len(e.cfg.PassEnv) == 0 {
		return nil
	}
	set := make(map[string]bool, len(env)+len(e.cfg.PassEnv))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		set[name] = true
	}
	var refs []string
	for _, name := range e.cfg.PassEnv {
		if set[name] || name == environment.SessionTokenEnv || name == environment.APIURLEnv {
			continue
		}
		set[name] = true
		if _, ok := os.LookupEnv(name); !ok {
			continue
		}
		refs = append(refs, name+"="+name)
	}
	return refs
}

// guestControlLink returns the guest workspace entry that resolves to
// the shared control directory once after_create has run. It is a path
// inside the VM, never opened on the host.
func guestControlLink() string {
	return path.Join(GuestWorkspace, workspacekit.SortieDir)
}

// linkExistsStatus is the exit status of the link script when the entry
// already exists.
const linkExistsStatus = 17

// linkScript returns the guest script that links link to the control
// directory unless an entry of any type, a dangling link included,
// already exists there, in which case it changes nothing and exits with
// linkExistsStatus.
func linkScript(link string) string {
	return fmt.Sprintf(`if [ -e '%[1]s' ] || [ -L '%[1]s' ]; then exit %[3]d; fi
exec ln -s '%[2]s' '%[1]s'`, link, GuestControlDir, linkExistsStatus)
}

// AfterCreate links /workspace/.sortie to the shared control directory
// in the workspace's VM, once after_create has succeeded in a VM
// created for the workspace, so the agent's control files reach the
// host workspace's .sortie directory and Sortie's reach the agent.
//
// When /workspace/.sortie already exists, for example because the
// repository after_create cloned commits one, it fails with an error
// naming the path and leaves the entry as it is. The link lives on the
// VM's disk, so a VM that is started again keeps it; it is not checked
// again then, since an agent that replaces it only hides Sortie's files
// from itself: Sortie reads and writes the host directory either way.
func (e *Environment) AfterCreate(ctx context.Context, ws environment.Workspace) error {
	name, err := execVMName(ws)
	if err != nil {
		return err
	}
	link := guestControlLink()
	_, err = e.cli.Exec(ctx, name, ExecOptions{Workdir: "/"}, []string{"sh", "-c", linkScript(link)})
	if cliErr, ok := errors.AsType[*CLIError](err); ok && cliErr.ExitCode == linkExistsStatus {
		return fmt.Errorf("guest workspace entry %s already exists, as after_create left it (a repository may commit one); "+
			"Sortie needs that path for its control directory and does not replace it", link)
	}
	if err != nil {
		return fmt.Errorf("link %s to %s in the VM: %w", link, GuestControlDir, err)
	}
	return nil
}

// validEnvName reports whether name is a POSIX environment variable name.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
