package agentcore

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestResolveLaunchTarget_Carrier(t *testing.T) {
	// Not parallel: t.Setenv empties PATH so no binary resolves on this host.
	t.Setenv("PATH", emptyDir(t))
	ws := t.TempDir()
	carrier := &agenttest.FakeCarrier{}
	session := []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "1"}}

	tests := []struct {
		name     string
		config   domain.AgentConfig
		fallback string
		wantCmd  string
		wantArgs []string
	}{
		{name: "string command splits on whitespace", config: domain.AgentConfig{Command: "opencode  acp --pure"}, wantCmd: "opencode", wantArgs: []string{"acp", "--pure"}},
		{name: "list command keeps its elements", config: domain.AgentConfig{CommandArgv: []string{"my agent", "app server"}}, wantCmd: "my agent", wantArgs: []string{"app server"}},
		{name: "default command", fallback: "codex app-server", wantCmd: "codex", wantArgs: []string{"app-server"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   tt.config,
				SessionEnv:    session,
				Carrier:       carrier,
			}, tt.fallback)
			if agentErr != nil {
				t.Fatalf("ResolveLaunchTarget() error = %v, want nil: nothing resolves on this host in carrier mode", agentErr)
			}
			if target.Command != tt.wantCmd || !slices.Equal(target.Args, tt.wantArgs) {
				t.Errorf("Command, Args = %q, %q, want %q, %q", target.Command, target.Args, tt.wantCmd, tt.wantArgs)
			}
			if target.RemoteCommand != "" || target.SSHHost != "" {
				t.Errorf("RemoteCommand, SSHHost = %q, %q, want both empty", target.RemoteCommand, target.SSHHost)
			}
			if !target.Remote() || !target.MCPInGuest() {
				t.Errorf("Remote(), MCPInGuest() = %v, %v, want true, true", target.Remote(), target.MCPInGuest())
			}
			if target.Carrier != carrier || !slices.Equal(target.SessionEnv, session) {
				t.Errorf("target = %+v, want the carrier and session env passed through", target)
			}
		})
	}

	t.Run("refuses an SSH host as well", func(t *testing.T) {
		_, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
			WorkspacePath: ws,
			AgentConfig:   domain.AgentConfig{Command: "opencode"},
			SSHHost:       "worker",
			Carrier:       carrier,
		}, "")
		if agentErr == nil || agentErr.Kind != domain.ErrAgentNotFound {
			t.Errorf("ResolveLaunchTarget() error = %v, want an agent_not_found error", agentErr)
		}
	})
	t.Run("refuses an empty command", func(t *testing.T) {
		_, agentErr := ResolveLaunchTarget(domain.StartSessionParams{WorkspacePath: ws, Carrier: carrier}, "  ")
		if agentErr == nil || agentErr.Kind != domain.ErrAgentNotFound {
			t.Errorf("ResolveLaunchTarget() error = %v, want an agent_not_found error", agentErr)
		}
	})
}

func TestLaunchTarget_RemoteAndMCPInGuest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		target         LaunchTarget
		wantRemote     bool
		wantMCPInGuest bool
	}{
		{name: "local", target: LaunchTarget{Command: "/bin/agent"}},
		{name: "ssh", target: LaunchTarget{Command: "/usr/bin/ssh", RemoteCommand: "agent", SSHHost: "h"}, wantRemote: true},
		{name: "carrier", target: LaunchTarget{Command: "agent", Carrier: &agenttest.FakeCarrier{}}, wantRemote: true, wantMCPInGuest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.target.Remote(); got != tt.wantRemote {
				t.Errorf("Remote() = %v, want %v", got, tt.wantRemote)
			}
			if got := tt.target.MCPInGuest(); got != tt.wantMCPInGuest {
				t.Errorf("MCPInGuest() = %v, want %v", got, tt.wantMCPInGuest)
			}
		})
	}
}

func TestLaunchTarget_GuestPath(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	mcp := filepath.Join(ws, ".sortie", "mcp.json")
	outside := filepath.Join(t.TempDir(), "operator-mcp.json")
	carrier := &agenttest.FakeCarrier{HostWorkspace: ws, GuestWorkspace: "/workspace"}

	tests := []struct {
		name   string
		target LaunchTarget
		in     string
		want   string
	}{
		{name: "local is the identity", target: LaunchTarget{Command: "/bin/agent", WorkspacePath: ws}, in: mcp, want: mcp},
		{name: "ssh is the identity", target: LaunchTarget{Command: "/usr/bin/ssh", WorkspacePath: ws, RemoteCommand: "agent", SSHHost: "h"}, in: mcp, want: mcp},
		{name: "carrier maps the control directory", target: LaunchTarget{Command: "agent", WorkspacePath: ws, Carrier: carrier}, in: mcp, want: "/workspace/.sortie/mcp.json"},
		{name: "carrier keeps a path outside the workspace", target: LaunchTarget{Command: "agent", WorkspacePath: ws, Carrier: carrier}, in: outside, want: outside},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.target.GuestPath(tt.in); got != tt.want {
				t.Errorf("GuestPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLaunchTarget_RemoteLaunch_SSHMatchesBuildSSHLaunch(t *testing.T) {
	// Not parallel: t.Setenv carries the SSH environment variable.
	t.Setenv("RL_CARRIED_VAR", "carried-value")
	target := LaunchTarget{
		Command:                  "/usr/bin/ssh",
		WorkspacePath:            "/work/space",
		RemoteCommand:            "opencode run",
		SSHHost:                  "worker-host",
		SSHStrictHostKeyChecking: "yes",
		SSHEnvNames:              []string{"RL_CARRIED_VAR"},
		WithheldEnv:              []string{"WITHHELD_VAR"},
		SessionEnv:               []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "1"}},
	}
	args := []string{"--format", "json"}
	settings := []sshutil.EnvVar{{Name: "MANAGED_SETTING", Value: "on"}}

	want := sshutil.BuildSSHLaunch(target.SSHHost, target.WorkspacePath, target.RemoteCommand, args, target.SSHOptions(settings...))
	got, agentErr := target.RemoteLaunch(args, settings...)
	if agentErr != nil {
		t.Fatalf("RemoteLaunch() error = %v, want nil", agentErr)
	}
	if got.Path != target.Command || !slices.Equal(got.Args, want.Args) {
		t.Errorf("RemoteLaunch() = %q %q, want %q %q", got.Path, got.Args, target.Command, want.Args)
	}
	gotStdin, err := io.ReadAll(got.StdinReader())
	if err != nil {
		t.Fatalf("ReadAll(StdinReader()): %v", err)
	}
	wantStdin, err := io.ReadAll(want.StdinReader())
	if err != nil {
		t.Fatalf("ReadAll(want.StdinReader()): %v", err)
	}
	if string(gotStdin) != string(wantStdin) || len(gotStdin) == 0 {
		t.Errorf("StdinReader() = %d bytes, want the %d-byte preamble BuildSSHLaunch builds", len(gotStdin), len(wantStdin))
	}
}

func TestLaunchTarget_RemoteLaunch_Carrier(t *testing.T) {
	t.Parallel()

	carrier := &agenttest.FakeCarrier{}
	target := LaunchTarget{
		Command:       "opencode",
		Args:          []string{"acp"},
		WorkspacePath: t.TempDir(),
		WithheldEnv:   []string{"WITHHELD_VAR", "SORTIE_RULE"},
		SessionEnv: []domain.EnvVar{
			{Name: "SORTIE_ISSUE_ID", Value: "1"},
			{Name: "SORTIE_RULE", Value: ""},
			{Name: "SORTIE_ISSUE_ID", Value: "shadowed"},
		},
		Carrier: carrier,
	}

	got, agentErr := target.RemoteLaunch([]string{"--pure"},
		sshutil.EnvVar{Name: "MANAGED_SETTING", Value: "on"},
		sshutil.EnvVar{Name: "EMPTY_SETTING", Value: ""},
		sshutil.EnvVar{Name: "WITHHELD_VAR", Value: "x"},
		sshutil.EnvVar{Name: "SORTIE_ISSUE_ID", Value: "overridden"},
		sshutil.EnvVar{Name: "MANAGED_SETTING", Value: "again"},
	)
	if agentErr != nil {
		t.Fatalf("RemoteLaunch() error = %v, want nil", agentErr)
	}
	launches := carrier.Launches()
	if len(launches) != 1 {
		t.Fatalf("carrier launches = %d, want 1", len(launches))
	}
	l := launches[0]
	if want := []string{"opencode", "acp", "--pure"}; !slices.Equal(l.Argv, want) {
		t.Errorf("launch Argv = %q, want %q", l.Argv, want)
	}
	if want := []string{"SORTIE_ISSUE_ID=1", "SORTIE_RULE="}; !slices.Equal(l.Env, want) {
		t.Errorf("launch Env = %q, want %q", l.Env, want)
	}
	if want := []string{"MANAGED_SETTING=on"}; !slices.Equal(l.AgentEnv, want) {
		t.Errorf("launch AgentEnv = %q, want %q", l.AgentEnv, want)
	}
	if want := []string{"WITHHELD_VAR"}; !slices.Equal(l.Unset, want) {
		t.Errorf("launch Unset = %q, want %q", l.Unset, want)
	}
	if got.StdinReader() != nil {
		t.Error("StdinReader() is non-nil, want no preamble: a carrier never writes the agent's standard input")
	}
	if !strings.HasSuffix(got.Path, "env") || !slices.Contains(got.Args, "MANAGED_SETTING=on") || got.Args[len(got.Args)-1] != "--pure" {
		t.Errorf("RemoteLaunch() = %q %q, want the carrier's argv", got.Path, got.Args)
	}
}

func TestLaunchTarget_RemoteLaunch_CarrierError(t *testing.T) {
	t.Parallel()

	cause := errors.New("no such machine")
	target := LaunchTarget{Command: "opencode", WorkspacePath: t.TempDir(), Carrier: &agenttest.FakeCarrier{Err: cause}}
	_, agentErr := target.RemoteLaunch(nil)
	if agentErr == nil || agentErr.Kind != domain.ErrPortExit || !errors.Is(agentErr, cause) {
		t.Errorf("RemoteLaunch() error = %v, want a port_exit error wrapping %v", agentErr, cause)
	}
}

func TestLaunchTarget_AuxiliaryCommand_Carrier(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	carrier := &agenttest.FakeCarrier{}
	target := LaunchTarget{
		Command:       "opencode",
		WorkspacePath: ws,
		SessionEnv:    []domain.EnvVar{{Name: "SORTIE_SESSION_TOKEN", Value: "tok"}},
		Carrier:       carrier,
	}
	cmd, agentErr := target.AuxiliaryCommand(context.Background(), []string{"session", "delete"}, strings.NewReader("in"), nil,
		sshutil.EnvVar{Name: "OPENCODE_DISABLE_AUTOUPDATE", Value: "true"})
	if agentErr != nil {
		t.Fatalf("AuxiliaryCommand() error = %v, want nil", agentErr)
	}
	if !strings.HasSuffix(cmd.Path, "env") || !slices.Equal(cmd.Args[len(cmd.Args)-3:], []string{"opencode", "session", "delete"}) {
		t.Errorf("AuxiliaryCommand() = %q %q, want the carrier's argv", cmd.Path, cmd.Args)
	}
	if cmd.Dir != ws {
		t.Errorf("Dir = %q, want %q", cmd.Dir, ws)
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil || string(got) != "in" {
		t.Errorf("Stdin = %q, %v, want the caller's own stdin and no preamble", got, err)
	}
	if !slices.Contains(cmd.Env, "SORTIE_SESSION_TOKEN=tok") {
		t.Error("host process env lacks the session variables: a host prefix needs them")
	}
	if l := carrier.Launches(); len(l) != 1 || !slices.Equal(l[0].AgentEnv, []string{"OPENCODE_DISABLE_AUTOUPDATE=true"}) {
		t.Errorf("carrier launches = %+v, want one carrying the adapter setting", l)
	}

	failing := target
	failing.Carrier = &agenttest.FakeCarrier{Err: errors.New("boom")}
	if cmd, agentErr := failing.AuxiliaryCommand(context.Background(), nil, nil, nil); agentErr == nil || cmd != nil {
		t.Errorf("AuxiliaryCommand() = %v, %v, want nil and an error", cmd, agentErr)
	}
}

func TestLaunchTarget_ConnectionFailed(t *testing.T) {
	t.Parallel()

	carrier := &agenttest.FakeCarrier{ConnectionFailedExit: 1, ConnectionFailedStderr: "vm not found"}
	tail := func() []string { return []string{"Error: vm not found: sortie-x"} }
	tests := []struct {
		name     string
		target   LaunchTarget
		exitCode int
		tail     func() []string
		want     bool
		wantErr  error
	}{
		{name: "local never", target: LaunchTarget{Command: "/bin/agent"}, exitCode: 255, tail: tail},
		{name: "ssh 255", target: LaunchTarget{RemoteCommand: "agent"}, exitCode: 255, want: true, wantErr: sshutil.ErrConnectionFailed},
		{name: "ssh other status", target: LaunchTarget{RemoteCommand: "agent"}, exitCode: 1, tail: tail},
		{name: "carrier classifies with stderr", target: LaunchTarget{Command: "agent", Carrier: carrier}, exitCode: 1, tail: tail, want: true, wantErr: ErrEnvironmentConnectionFailed},
		{name: "carrier without stderr", target: LaunchTarget{Command: "agent", Carrier: carrier}, exitCode: 1},
		{name: "carrier ignores 255", target: LaunchTarget{Command: "agent", Carrier: carrier}, exitCode: 255, tail: tail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.target.ConnectionFailed(tt.exitCode, tt.tail); got != tt.want {
				t.Errorf("ConnectionFailed(%d) = %v, want %v", tt.exitCode, got, tt.want)
			}
			if tt.wantErr == nil {
				return
			}
			connErr := tt.target.ConnectionFailedError()
			if connErr.Kind != domain.ErrPortExit || !errors.Is(connErr, tt.wantErr) || !IsConnectionFailure(connErr) {
				t.Errorf("ConnectionFailedError() = %v, want a port_exit error wrapping %v", connErr, tt.wantErr)
			}
		})
	}
}

func TestTargetReaperConnectionFailed_Carrier(t *testing.T) {
	t.Parallel()

	carrier := &agenttest.FakeCarrier{ConnectionFailedExit: 1, ConnectionFailedStderr: "vm not found"}
	target := LaunchTarget{Command: "agent", Carrier: carrier}
	r := reapedReaper(t, agenttest.Output{ExitCode: 1})
	if !TargetReaperConnectionFailed(target, r, time.Second, func() []string { return []string{"Error: vm not found"} }) {
		t.Error("TargetReaperConnectionFailed() = false, want the carrier's verdict")
	}
	if TargetReaperConnectionFailed(LaunchTarget{Command: "agent"}, r, time.Second, nil) {
		t.Error("TargetReaperConnectionFailed(local) = true, want false")
	}
}

func TestEarlyExit_Report_CarrierConnectionFailure(t *testing.T) {
	t.Parallel()

	carrier := &agenttest.FakeCarrier{ConnectionFailedExit: 1, ConnectionFailedStderr: "vm not found"}
	target := LaunchTarget{Command: "agent", Carrier: carrier}
	r := reapedReaper(t, agenttest.Output{ExitCode: 1, Stderr: "Error: vm not found: sortie-x\n"})
	stderr := procutilCollector(t, "Error: vm not found: sortie-x\n")

	got := ObserveEarlyExit(context.Background(), target, r, time.Second).Report(stderr)
	if got == nil || got.Kind != domain.ErrPortExit || !errors.Is(got, ErrEnvironmentConnectionFailed) {
		t.Fatalf("Report() = %v, want a port_exit error wrapping ErrEnvironmentConnectionFailed", got)
	}
	if tails := carrier.StderrTails(); len(tails) != 1 || !slices.ContainsFunc(tails[0], func(l string) bool { return strings.Contains(l, "vm not found") }) {
		t.Errorf("carrier stderr tails = %q, want the launch's stderr", tails)
	}

	r = reapedReaper(t, agenttest.Output{ExitCode: 1, Stderr: "agent failed\n"})
	got = ObserveEarlyExit(context.Background(), target, r, time.Second).Report(procutilCollector(t, "agent failed\n"))
	if got == nil || IsConnectionFailure(got) {
		t.Errorf("Report() = %v, want the agent's own early exit, not a connection failure", got)
	}
}

// procutilCollector returns a collector that has already drained s.
func procutilCollector(t *testing.T, s string) *procutil.StderrCollector {
	t.Helper()
	return procutil.NewStderrCollector(strings.NewReader(s), nil)
}
