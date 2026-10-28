package agentcore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// LaunchTarget captures the resolved launch parameters for a single agent
// session. It is populated once by [ResolveLaunchTarget] during
// [domain.AgentAdapter.StartSession] and stored in adapter session state.
// RunTurn uses it to construct the subprocess command.
//
// When RemoteCommand is non-empty the session runs in SSH mode: Command is
// the local ssh binary, SSHHost is the remote destination, and RemoteCommand
// is the agent command to execute there. When Carrier is non-nil the session
// runs in an execution environment: Command and Args are the agent command
// as in local mode, but Command is resolved inside the environment, and
// Carrier builds the host process that runs it there. Otherwise the session
// runs locally: Command is the resolved agent binary and Args contains any
// initial CLI arguments (e.g., ["app-server"]).
//
// SSH and carrier mode are both remote ([LaunchTarget.Remote]): the agent
// binary is not on this host. Every remote launch is built through
// [LaunchTarget.RemoteLaunch], and every launch of the session itself
// through [LaunchTarget.SessionLaunch], which puts HostPrefix in front of
// it in any mode.
type LaunchTarget struct {
	// Command is the resolved path to the local binary to exec.
	// In SSH mode this is the path to the ssh binary.
	// In local mode this is the path to the agent binary, except with a
	// HostPrefix, where it is the agent command's first word unresolved,
	// as it would be had the prefix been written in front of the agent
	// command.
	// In carrier mode this is the agent command's first word, resolved
	// inside the environment.
	Command string

	// Args contains initial CLI arguments inserted before per-turn
	// arguments. Non-empty only in local and carrier mode when the
	// configured command holds more than its executable: the words after
	// the first of a string command (e.g., "codex app-server" yields Args:
	// ["app-server"]), or the elements after element zero of a list
	// command. Empty in SSH mode.
	Args []string

	// WorkspacePath is the validated absolute path to the agent workspace
	// directory. Both local and SSH mode set this field.
	WorkspacePath string

	// RemoteCommand is the agent command string to run on the SSH host.
	// Non-empty signals SSH mode. Empty in local mode.
	RemoteCommand string

	// SSHHost is the SSH destination string after whitespace trimming.
	// Non-empty when RemoteCommand is non-empty.
	SSHHost string

	// SSHStrictHostKeyChecking is the OpenSSH StrictHostKeyChecking value
	// passed through from StartSessionParams. Empty means the SSH caller
	// defaults to "accept-new".
	SSHStrictHostKeyChecking string

	// SSHEnvNames lists the environment variable names to carry into
	// a remote session, passed through from StartSessionParams. Empty
	// in local mode.
	SSHEnvNames []string

	// WithheldEnv lists environment variable names no launch through this
	// target carries: a local launch runs without them, and a remote launch
	// neither carries them nor lets the remote shell pass them to the agent.
	WithheldEnv []string

	// SessionEnv lists the session identity variables every launch
	// through this target sets, passed through from StartSessionParams.
	// [LaunchTarget.BindWorkspace] sets them last, and
	// [LaunchTarget.SSHOptions] carries them last, so each wins over a
	// variable of the same name from any other source.
	SessionEnv []domain.EnvVar

	// Carrier, when non-nil, runs the session inside the execution
	// environment it stands for, passed through from StartSessionParams.
	// Nil in local and SSH mode.
	Carrier domain.LaunchCarrier

	// HostPrefix, when non-empty, is the argv [LaunchTarget.SessionLaunch]
	// runs on this host in front of every launch of the session, passed
	// through from StartSessionParams with element zero resolved on this
	// host. No auxiliary command runs behind it.
	HostPrefix []string

	// StopGrace is the session's resolved agent.stop_grace_ms, which a remote
	// launch also grants its runtime once the SSH connection is gone. Zero in
	// local mode.
	StopGrace time.Duration
}

// Remote reports whether the agent binary is not on this host, so no
// local canary or binary lookup applies to it: true in SSH and carrier
// mode.
func (t LaunchTarget) Remote() bool {
	return t.RemoteCommand != "" || t.Carrier != nil
}

// MCPInGuest reports whether the MCP configuration Sortie generated in
// the workspace is valid where the agent runs, so a remote launch may
// pass it to the agent: true in carrier mode only. A local launch reads
// it on this host; an SSH launch gets no MCP servers.
func (t LaunchTarget) MCPInGuest() bool {
	return t.Carrier != nil
}

// GuestPath returns the path at which the agent sees hostPath, a path in
// the workspace on this host: the carrier's mapping in carrier mode, and
// hostPath itself in local and SSH mode.
func (t LaunchTarget) GuestPath(hostPath string) string {
	if t.Carrier == nil {
		return hostPath
	}
	return t.Carrier.GuestPath(hostPath)
}

// ResolveLaunchTarget resolves the workspace path and agent binary for a
// session, choosing between carrier, SSH, and local launch modes based on
// params.Carrier and params.SSHHost. It returns a populated [LaunchTarget]
// and a nil error on success, or a zero [LaunchTarget] and a
// [*domain.AgentError] on failure.
//
// defaultCommand is the fallback agent command when params.AgentConfig holds
// no command (e.g., "claude", "copilot", "codex app-server"). A non-empty
// params.AgentConfig.CommandArgv is the command instead: locally element zero
// resolves whole, whitespace included, and the rest become Args; over SSH
// every element is single-quoted into RemoteCommand. This is the only reader
// of CommandArgv. In carrier mode the command splits as it does locally, but
// nothing is resolved on this host: the environment resolves it.
//
// A non-empty params.HostPrefix has its element zero resolved on this host
// and becomes the target's HostPrefix. In local mode the agent command then
// splits as it does in carrier mode and stays unresolved, so the session
// launch is exactly the one a prefix written in front of the agent command
// would yield: only the prefix's program is looked up.
//
// Adapter-specific post-resolution steps (canary version checks, auth
// preflights) are the caller's responsibility and must run after
// ResolveLaunchTarget returns successfully.
func ResolveLaunchTarget(params domain.StartSessionParams, defaultCommand string) (LaunchTarget, *domain.AgentError) {
	absPath, agentErr := ResolveWorkspace(params.WorkspacePath)
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}

	hostPrefix, agentErr := resolveHostPrefix(params.HostPrefix)
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}

	target, agentErr := resolveLaunchTarget(params, absPath, defaultCommand)
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}
	target.HostPrefix = hostPrefix
	return target, nil
}

// resolveHostPrefix returns prefix with element zero resolved on this
// host, or nil when prefix is empty.
func resolveHostPrefix(prefix []string) ([]string, *domain.AgentError) {
	if len(prefix) == 0 {
		return nil, nil
	}
	resolved, err := exec.LookPath(prefix[0])
	if err != nil {
		return nil, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: fmt.Sprintf("host prefix command %q not found", prefix[0]),
			Err:     err,
		}
	}
	return append([]string{resolved}, prefix[1:]...), nil
}

func resolveLaunchTarget(params domain.StartSessionParams, absPath, defaultCommand string) (LaunchTarget, *domain.AgentError) {
	sshHost := strings.TrimSpace(params.SSHHost)

	if params.Carrier != nil || (sshHost == "" && len(params.HostPrefix) > 0) {
		return resolveUnresolvedLaunch(params, absPath, sshHost, defaultCommand)
	}

	if argv := params.AgentConfig.CommandArgv; len(argv) > 0 {
		return resolveArgvLaunch(params, absPath, sshHost, argv)
	}

	command := cmp.Or(params.AgentConfig.Command, defaultCommand)

	if strings.TrimSpace(command) == "" {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "agent command is empty or whitespace-only",
		}
	}

	if sshHost != "" {
		return resolveSSHLaunch(params, absPath, sshHost, command)
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "agent command is empty or whitespace-only",
		}
	}

	resolved, agentErr := ResolveBinary(parts[0])
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}

	return LaunchTarget{
		Command:       resolved,
		Args:          slices.Clone(parts[1:]),
		WorkspacePath: absPath,
		SessionEnv:    slices.Clone(params.SessionEnv),
	}, nil
}

func resolveSSHLaunch(params domain.StartSessionParams, absPath, sshHost, remoteCommand string) (LaunchTarget, *domain.AgentError) {
	sshPath, lookErr := exec.LookPath("ssh")
	if lookErr != nil {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "ssh binary not found on orchestrator host",
			Err:     lookErr,
		}
	}
	return LaunchTarget{
		Command:                  sshPath,
		Args:                     nil,
		WorkspacePath:            absPath,
		RemoteCommand:            remoteCommand,
		SSHHost:                  sshHost,
		SSHStrictHostKeyChecking: params.SSHStrictHostKeyChecking,
		SSHEnvNames:              params.SSHEnvNames,
		SessionEnv:               slices.Clone(params.SessionEnv),
		StopGrace:                procutil.StopGrace(params.AgentConfig.StopGraceMS),
	}, nil
}

// resolveUnresolvedLaunch resolves a carrier-mode target, or a local one
// behind a host prefix: the agent command split as a local launch splits
// it, with no binary resolved on this host.
func resolveUnresolvedLaunch(params domain.StartSessionParams, absPath, sshHost, defaultCommand string) (LaunchTarget, *domain.AgentError) {
	if sshHost != "" {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "an agent session cannot run both over SSH and in an execution environment",
		}
	}
	argv := params.AgentConfig.CommandArgv
	if len(argv) == 0 {
		argv = strings.Fields(cmp.Or(params.AgentConfig.Command, defaultCommand))
	}
	if len(argv) == 0 || argv[0] == "" {
		return LaunchTarget{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: "agent command is empty or whitespace-only",
		}
	}
	return LaunchTarget{
		Command:       argv[0],
		Args:          slices.Clone(argv[1:]),
		WorkspacePath: absPath,
		SessionEnv:    slices.Clone(params.SessionEnv),
		Carrier:       params.Carrier,
	}, nil
}

func resolveArgvLaunch(params domain.StartSessionParams, absPath, sshHost string, argv []string) (LaunchTarget, *domain.AgentError) {
	if sshHost != "" {
		return resolveSSHLaunch(params, absPath, sshHost, sshutil.QuoteArgv(argv))
	}
	resolved, agentErr := lookPath(argv[0])
	if agentErr != nil {
		return LaunchTarget{}, agentErr
	}
	return LaunchTarget{
		Command:       resolved,
		Args:          slices.Clone(argv[1:]),
		WorkspacePath: absPath,
		SessionEnv:    slices.Clone(params.SessionEnv),
	}, nil
}

// AppendCommandArgs returns command, which names an executable,
// followed by args, in the form that launches exactly that argv. A list
// stays a list in both launch modes. A string launched locally stays a
// string only when every argument is non-empty and holds no space rune,
// since the local launch splits it on whitespace; otherwise it becomes
// the list of its whitespace-separated words followed by args. A string
// launched remotely stays a string with args quoted for the remote
// shell and placed where the launch places its own arguments.
func AppendCommandArgs(command domain.AgentCommand, remote bool, args ...string) domain.AgentCommand {
	switch {
	case len(command.Argv) > 0:
		return domain.AgentCommand{Argv: append(slices.Clone(command.Argv), args...)}
	case remote:
		return domain.AgentCommand{Line: sshutil.AppendArgs(command.Line, args...)}
	case slices.ContainsFunc(args, breaksWhitespaceSplit):
		return domain.AgentCommand{Argv: append(strings.Fields(command.Line), args...)}
	}
	words := append([]string{strings.TrimRightFunc(command.Line, unicode.IsSpace)}, args...)
	return domain.AgentCommand{Line: strings.Join(words, " ")}
}

// breaksWhitespaceSplit reports whether a whitespace split would not
// yield arg as one word: it is empty or holds a space rune.
func breaksWhitespaceSplit(arg string) bool {
	return arg == "" || strings.ContainsFunc(arg, unicode.IsSpace)
}

// SSHOptions resolves t's SSH transport options for one remote
// launch. It reads each of t.SSHEnvNames with [os.LookupEnv], in
// order, carrying a name only when its value holds a non-whitespace
// character, then appends every settings entry whose Value is
// non-empty. A name already carried, whether from t.SSHEnvNames or
// from an earlier settings entry, is not carried again.
//
// A variable that is unset, empty, or only whitespace is left behind,
// so a blank value on the orchestrator cannot override the credential
// or login the remote host already holds.
//
// settings carries adapter-owned variables with computed values, such
// as a tool-server setting. It MUST NOT carry a credential: a
// credential reaches a remote launch only as a name in
// t.SSHEnvNames. SSHOptions calls os.LookupEnv on every call, so
// calling it once per launch reflects the orchestrator's environment
// at that moment.
//
// A name in t.WithheldEnv is never carried, whether t.SSHEnvNames or a
// settings entry names it, and is returned as the options' Unset list.
//
// Every entry of t.SessionEnv is carried last, even with an empty
// Value, and replaces any entry of the same name the other sources
// would carry; its name is left out of the Unset list, so the remote
// agent receives exactly the value a local launch would set.
func (t LaunchTarget) SSHOptions(settings ...sshutil.EnvVar) sshutil.SSHOptions {
	var carried []sshutil.EnvVar
	sessionNames := make(map[string]bool, len(t.SessionEnv))
	for _, entry := range t.SessionEnv {
		sessionNames[entry.Name] = true
	}
	skip := make(map[string]bool, len(t.WithheldEnv)+len(t.SSHEnvNames)+len(settings)+len(sessionNames))
	for _, name := range t.WithheldEnv {
		skip[name] = true
	}
	for _, entry := range settings {
		skip[entry.Name] = true
	}
	for name := range sessionNames {
		skip[name] = true
	}

	for _, name := range t.SSHEnvNames {
		if skip[name] {
			continue
		}
		skip[name] = true
		value, present := os.LookupEnv(name)
		if !present || strings.TrimSpace(value) == "" {
			continue
		}
		carried = append(carried, sshutil.EnvVar{Name: name, Value: value})
	}

	added := make(map[string]bool, len(settings))
	for _, entry := range settings {
		if entry.Value == "" || added[entry.Name] || slices.Contains(t.WithheldEnv, entry.Name) || sessionNames[entry.Name] {
			continue
		}
		added[entry.Name] = true
		carried = append(carried, entry)
	}

	for _, entry := range t.SessionEnv {
		if added[entry.Name] {
			continue
		}
		added[entry.Name] = true
		carried = append(carried, sshutil.EnvVar{Name: entry.Name, Value: entry.Value})
	}

	var unset []string
	if len(t.WithheldEnv) > 0 {
		unset = slices.DeleteFunc(slices.Clone(t.WithheldEnv), func(name string) bool { return sessionNames[name] })
	}

	return sshutil.SSHOptions{
		StrictHostKeyChecking: t.SSHStrictHostKeyChecking,
		Env:                   carried,
		Unset:                 unset,
		StopGrace:             t.StopGrace,
	}
}

// HostLaunch is the host process of one remote launch: the program to
// exec on this host, its arguments, and, for an SSH launch that carries
// variables, the standard input preamble the remote shell reads ahead of
// the agent's own bytes. Obtain one from [LaunchTarget.RemoteLaunch].
type HostLaunch struct {
	// Path is the program to exec on this host.
	Path string

	// Args is the program's argument vector after Path, suitable for
	// exec.Command's variadic arguments.
	Args []string

	ssh sshutil.SSHLaunch
}

// StdinReader returns a reader yielding the launch's standard input
// preamble, or a nil [io.Reader] interface value when it carries none,
// as [sshutil.SSHLaunch.StdinReader] does. Only an SSH launch carries
// one.
func (l HostLaunch) StdinReader() io.Reader {
	return l.ssh.StdinReader()
}

// PrefixStdin returns w with the launch's standard input preamble sent
// ahead of the caller's first write, as [sshutil.SSHLaunch.PrefixStdin]
// does, and w unchanged when the launch carries none.
func (l HostLaunch) PrefixStdin(w io.WriteCloser) io.WriteCloser {
	return l.ssh.PrefixStdin(w)
}

// WatchStderr returns r, through which the launch reads a reverse
// forward's outcome from the host process's standard error, as
// [sshutil.SSHLaunch.WatchStderr] does. A launch without a forward
// returns r unchanged.
func (l HostLaunch) WatchStderr(r io.Reader) io.Reader {
	return l.ssh.WatchStderr(r)
}

// WaitOutcome waits for the outcome of the launch's reverse forward, as
// [sshutil.SSHLaunch.WaitOutcome] does.
func (l HostLaunch) WaitOutcome(ctx context.Context) (sshutil.ForwardOutcome, error) {
	return l.ssh.WaitOutcome(ctx)
}

// RemoteLaunch builds the host process that runs the agent command with
// args appended at a remote target. In SSH mode it is the ssh launch
// [sshutil.BuildSSHLaunch] builds from t.SSHOptions(settings...). In
// carrier mode it is the argv t.Carrier returns for Command, Args, and
// args, carrying t.SessionEnv as the session's identity variables and
// every settings entry with a non-empty Value, except a name in
// t.WithheldEnv or t.SessionEnv, as an agent variable; a name in
// t.WithheldEnv that t.SessionEnv does not set is passed for unsetting.
// settings follows the contract of [LaunchTarget.SSHOptions]: it MUST
// NOT carry a credential.
//
// It must not be called in local mode. A carrier that cannot build the
// launch yields a port_exit [*domain.AgentError].
func (t LaunchTarget) RemoteLaunch(args []string, settings ...sshutil.EnvVar) (HostLaunch, *domain.AgentError) {
	return t.remoteLaunch(args, nil, settings)
}

// remoteLaunch is [LaunchTarget.RemoteLaunch] with forward, the reverse
// forward an SSH launch requests. A carrier launch ignores forward.
func (t LaunchTarget) remoteLaunch(args []string, forward *sshutil.ReverseForward, settings []sshutil.EnvVar) (HostLaunch, *domain.AgentError) {
	if t.Carrier == nil {
		opts := t.SSHOptions(settings...)
		opts.Forward = forward
		launch := sshutil.BuildSSHLaunch(t.SSHHost, t.WorkspacePath, t.RemoteCommand, args, opts)
		return HostLaunch{Path: t.Command, Args: launch.Args, ssh: launch}, nil
	}

	argv := make([]string, 0, 1+len(t.Args)+len(args))
	argv = append(argv, t.Command)
	argv = append(argv, t.Args...)
	argv = append(argv, args...)

	sessionNames := make(map[string]bool, len(t.SessionEnv))
	env := make([]string, 0, len(t.SessionEnv))
	for _, entry := range t.SessionEnv {
		if sessionNames[entry.Name] {
			continue
		}
		sessionNames[entry.Name] = true
		env = append(env, entry.Name+"="+entry.Value)
	}
	var agentEnv []string
	added := make(map[string]bool, len(settings))
	for _, entry := range settings {
		if entry.Value == "" || added[entry.Name] || slices.Contains(t.WithheldEnv, entry.Name) || sessionNames[entry.Name] {
			continue
		}
		added[entry.Name] = true
		agentEnv = append(agentEnv, entry.Name+"="+entry.Value)
	}
	var unset []string
	if len(t.WithheldEnv) > 0 {
		unset = slices.DeleteFunc(slices.Clone(t.WithheldEnv), func(name string) bool { return sessionNames[name] })
	}

	hostArgv, err := t.Carrier.LaunchArgv(domain.CarrierLaunch{Argv: argv, Env: env, AgentEnv: agentEnv, Unset: unset})
	if err == nil && len(hostArgv) == 0 {
		err = errors.New("the execution environment returned an empty launch command")
	}
	if err != nil {
		return HostLaunch{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "build the execution environment launch",
			Err:     err,
		}
	}
	return HostLaunch{Path: hostArgv[0], Args: hostArgv[1:]}, nil
}

// SessionLaunch builds the host process of one launch of the agent
// session itself, with args appended: in local mode Command with Args and
// args, and in SSH and carrier mode what [LaunchTarget.RemoteLaunch]
// builds from args and settings. settings applies to a remote launch
// only, under the same contract. With a HostPrefix, the prefix runs in
// front: Path is HostPrefix[0], and Args are the rest of the prefix
// followed by the launch's own Path and Args, so the prefix process is
// the one started on this host and inherits the environment the caller
// binds to it. Every adapter launches its session, the credential check
// included, through SessionLaunch; an auxiliary command never does.
//
// It fails only where [LaunchTarget.RemoteLaunch] does.
func (t LaunchTarget) SessionLaunch(args []string, settings ...sshutil.EnvVar) (HostLaunch, *domain.AgentError) {
	return t.ForwardedSessionLaunch(args, nil, settings...)
}

// ForwardedSessionLaunch is [LaunchTarget.SessionLaunch] for a session
// whose SSH launch requests forward, the reverse forward that carries
// Sortie's tool server to the remote host. A nil forward, a local launch,
// and a carrier launch request none.
func (t LaunchTarget) ForwardedSessionLaunch(args []string, forward *sshutil.ReverseForward, settings ...sshutil.EnvVar) (HostLaunch, *domain.AgentError) {
	var launch HostLaunch
	if t.Remote() {
		var agentErr *domain.AgentError
		launch, agentErr = t.remoteLaunch(args, forward, settings)
		if agentErr != nil {
			return HostLaunch{}, agentErr
		}
	} else {
		launch = HostLaunch{Path: t.Command, Args: append(slices.Clone(t.Args), args...)}
	}
	if len(t.HostPrefix) == 0 {
		return launch, nil
	}
	prefixed := make([]string, 0, len(t.HostPrefix)+len(launch.Args))
	prefixed = append(prefixed, t.HostPrefix[1:]...)
	prefixed = append(prefixed, launch.Path)
	prefixed = append(prefixed, launch.Args...)
	launch.Path = t.HostPrefix[0]
	launch.Args = prefixed
	return launch, nil
}

// ConnectionFailed reports whether a remote launch's host process that
// exited with exitCode failed to reach the agent rather than the agent
// failing: OpenSSH's connection-failure status in SSH mode, and the
// carrier's verdict in carrier mode. stderrTail, when non-nil, returns
// the host process's last standard error lines; only a carrier calls it.
// It is always false in local mode.
func (t LaunchTarget) ConnectionFailed(exitCode int, stderrTail func() []string) bool {
	if !t.Remote() {
		return false
	}
	return remoteConnectionFailed(t.Carrier, exitCode, stderrTail)
}

// remoteConnectionFailed classifies a remote launch's exit: by carrier
// when it is non-nil, and as an SSH launch otherwise.
func remoteConnectionFailed(carrier domain.LaunchCarrier, exitCode int, stderrTail func() []string) bool {
	if carrier == nil {
		return sshutil.ConnectionFailed(exitCode)
	}
	var tail []string
	if stderrTail != nil {
		tail = stderrTail()
	}
	return carrier.ConnectionFailed(exitCode, tail)
}

// ConnectionFailedError returns the error for a remote launch that
// [LaunchTarget.ConnectionFailed] classified as failing to reach the
// agent: [ConnectionFailedError] in SSH mode, and
// [EnvironmentConnectionFailedError] in carrier mode.
func (t LaunchTarget) ConnectionFailedError() *domain.AgentError {
	return remoteConnectionFailedError(t.Carrier)
}

func remoteConnectionFailedError(carrier domain.LaunchCarrier) *domain.AgentError {
	if carrier != nil {
		return EnvironmentConnectionFailedError()
	}
	return ConnectionFailedError()
}

// RuntimeEnv returns name's value as the runtime receives it from
// Sortie: from env, or [os.Environ] when env is nil, for a local
// launch; for a remote launch, only a variable the launch forwards
// over SSH (none in carrier mode), which is a name in t.SSHEnvNames whose value in env holds a
// non-whitespace character, the rule [LaunchTarget.SSHOptions] carries
// by. A value the remote host sets for itself is invisible. A name in
// t.WithheldEnv is never visible. When env holds a name more than once
// the last entry wins, as it does for a launched process.
func (t LaunchTarget) RuntimeEnv(env []string, name string) (string, bool) {
	if slices.Contains(t.WithheldEnv, name) {
		return "", false
	}
	remote := t.Remote()
	if remote && !slices.Contains(t.SSHEnvNames, name) {
		return "", false
	}
	if env == nil {
		env = os.Environ()
	}
	var value string
	var found bool
	for _, entry := range env {
		if !isEnvEntryNamed(entry, name) {
			continue
		}
		_, value, _ = strings.Cut(entry, "=")
		found = true
	}
	if remote && strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, found
}

// BindWorkspace re-verifies t.WorkspacePath and, on success, sets
// cmd.Dir to it, sets cmd.Env to os.Environ() when it is nil, and
// replaces every PWD entry already in cmd.Env with one naming the same
// verified path, so a subprocess that trusts PWD over its own working
// directory still lands in the workspace. It also removes every entry
// named in t.WithheldEnv from cmd.Env, and then sets every entry of
// t.SessionEnv last, replacing any entry of the same name. On failure
// cmd.Dir and cmd.Env are left untouched and the verification error is
// returned; the caller must not start cmd.
//
// Callers MUST assign cmd.Env before calling BindWorkspace and MUST
// NOT assign it afterward, so the PWD and session variables this sets
// are never overwritten.
func (t LaunchTarget) BindWorkspace(cmd *exec.Cmd) *domain.AgentError {
	absPath, agentErr := ResolveWorkspace(t.WorkspacePath)
	if agentErr != nil {
		return agentErr
	}
	cmd.Dir = absPath
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = dropEnvNames(cmd.Env, t.WithheldEnv)
	cmd.Env = setPWD(cmd.Env, absPath)
	cmd.Env = setSessionEnv(cmd.Env, t.SessionEnv)
	return nil
}

// setSessionEnv returns env with every entry named by an entry of vars
// removed, compared as [isEnvEntryNamed] compares, and one NAME=value
// entry appended per name of vars, in order, keeping the first entry
// for a name vars repeats.
func setSessionEnv(env []string, vars []domain.EnvVar) []string {
	if len(vars) == 0 {
		return env
	}
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}
	env = dropEnvNames(env, names)
	seen := make(map[string]bool, len(vars))
	for _, v := range vars {
		if seen[v.Name] {
			continue
		}
		seen[v.Name] = true
		env = append(env, v.Name+"="+v.Value)
	}
	return env
}

// dropEnvNames returns env without the entries named in names, compared
// as [isEnvEntryNamed] compares.
func dropEnvNames(env, names []string) []string {
	if len(names) == 0 {
		return env
	}
	return slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		return slices.ContainsFunc(names, func(name string) bool {
			return isEnvEntryNamed(entry, name)
		})
	})
}

// setPWD returns env with every entry named PWD removed (case-
// insensitively on Windows, exactly elsewhere, matching the platform's
// own environment-variable name comparison) and exactly one
// "PWD=value" appended.
func setPWD(env []string, value string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if isPWDEntry(entry) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "PWD="+value)
}

// isPWDEntry reports whether entry is an env-slice assignment of PWD.
func isPWDEntry(entry string) bool {
	return isEnvEntryNamed(entry, "PWD")
}

// isEnvEntryNamed reports whether entry is an env-slice assignment of
// name, comparing case-insensitively on Windows to match that platform's
// own environment-variable name resolution.
func isEnvEntryNamed(entry, name string) bool {
	key, _, found := strings.Cut(entry, "=")
	if !found {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, name)
	}
	return key == name
}

// AuxiliaryCommand builds a one-shot runtime subcommand run in the
// workspace, locally, over ssh, or through the carrier like the session
// itself, but never behind HostPrefix. A nil env means [BindWorkspace] supplies os.Environ() with PWD
// replaced; stdin may be nil. Returns a non-nil [*domain.AgentError] and
// a nil *[exec.Cmd] when the workspace path fails re-verification
// immediately before launch, or when the carrier cannot build the
// launch.
func (t LaunchTarget) AuxiliaryCommand(ctx context.Context, args []string, stdin io.Reader, env []string, carried ...sshutil.EnvVar) (*exec.Cmd, *domain.AgentError) {
	var cmd *exec.Cmd
	if !t.Remote() {
		allArgs := append(slices.Clone(t.Args), args...)
		cmd = exec.CommandContext(ctx, t.Command, allArgs...) //nolint:gosec // args are constructed programmatically
		cmd.Stdin = stdin
	} else {
		launch, agentErr := t.RemoteLaunch(args, carried...)
		if agentErr != nil {
			return nil, agentErr
		}
		cmd = exec.CommandContext(ctx, launch.Path, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
		cmd.Stdin = prependPreamble(launch.StdinReader(), stdin)
	}
	cmd.Env = env
	if agentErr := t.BindWorkspace(cmd); agentErr != nil {
		return nil, agentErr
	}
	return cmd, nil
}

func prependPreamble(preamble, stdin io.Reader) io.Reader {
	switch {
	case preamble == nil:
		return stdin
	case stdin == nil:
		return preamble
	default:
		return io.MultiReader(preamble, stdin)
	}
}

// AuxiliaryTimeout returns the bound for an auxiliary launch: twice
// cfg.ReadTimeoutMS, capped at 30 seconds, and never non-positive.
func AuxiliaryTimeout(cfg domain.AgentConfig) time.Duration {
	return min(2*ReadTimeout(cfg), 30*time.Second)
}
