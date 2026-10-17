//go:build unix

package agentcore

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
)

// brokerOnPath puts a stand-in host prefix program named acp-broker
// alone on PATH, so no agent command resolves on this host, and returns
// its path.
func brokerOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	broker := agenttest.WriteScript(t, dir, "acp-broker", "exec \"$@\"\n")
	t.Setenv("PATH", dir)
	return broker
}

func TestSessionLaunch_HostPrefixLocalEqualsPrefixedAgentCommand(t *testing.T) {
	// Not parallel: brokerOnPath sets PATH.
	broker := brokerOnPath(t)
	ws := t.TempDir()
	prefix := []string{"acp-broker", "--"}
	turnArgs := []string{"--turn", "a b"}

	tests := []struct {
		name     string
		config   domain.AgentConfig
		prefixed domain.AgentConfig
		fallback string
		want     []string
	}{
		{
			name:     "string command",
			config:   domain.AgentConfig{Command: "opencode  acp"},
			prefixed: domain.AgentConfig{Command: "acp-broker -- opencode  acp"},
			want:     []string{"--", "opencode", "acp", "--turn", "a b"},
		},
		{
			name:     "list command",
			config:   domain.AgentConfig{CommandArgv: []string{"my agent", "acp"}},
			prefixed: domain.AgentConfig{CommandArgv: []string{"acp-broker", "--", "my agent", "acp"}},
			want:     []string{"--", "my agent", "acp", "--turn", "a b"},
		},
		{
			name:     "default command",
			fallback: "codex app-server",
			prefixed: domain.AgentConfig{Command: "acp-broker -- codex app-server"},
			want:     []string{"--", "codex", "app-server", "--turn", "a b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   tt.config,
				HostPrefix:    prefix,
			}, tt.fallback)
			if agentErr != nil {
				t.Fatalf("ResolveLaunchTarget(host prefix) error = %v, want nil: only the prefix resolves on this host", agentErr)
			}
			today, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
				WorkspacePath: ws,
				AgentConfig:   tt.prefixed,
			}, tt.fallback)
			if agentErr != nil {
				t.Fatalf("ResolveLaunchTarget(prefixed agent command) error = %v", agentErr)
			}
			if target.Remote() {
				t.Error("Remote() = true, want false: a host prefix keeps a local launch local")
			}

			got, agentErr := target.SessionLaunch(turnArgs)
			if agentErr != nil {
				t.Fatalf("SessionLaunch(host prefix) error = %v", agentErr)
			}
			want, agentErr := today.SessionLaunch(turnArgs)
			if agentErr != nil {
				t.Fatalf("SessionLaunch(prefixed agent command) error = %v", agentErr)
			}
			if got.Path != want.Path || !slices.Equal(got.Args, want.Args) {
				t.Errorf("SessionLaunch(host prefix) = %q %q, want %q %q: the launch of the prefix written into the agent command", got.Path, got.Args, want.Path, want.Args)
			}
			if got.Path != broker || !slices.Equal(got.Args, tt.want) {
				t.Errorf("SessionLaunch(host prefix) = %q %q, want %q %q", got.Path, got.Args, broker, tt.want)
			}

			gotCmd := exec.CommandContext(context.Background(), got.Path, got.Args...)
			wantCmd := exec.CommandContext(context.Background(), want.Path, want.Args...)
			if gotCmd.Path != wantCmd.Path || !slices.Equal(gotCmd.Args, wantCmd.Args) {
				t.Errorf("process = %q %q, want %q %q", gotCmd.Path, gotCmd.Args, wantCmd.Path, wantCmd.Args)
			}
		})
	}
}

func TestResolveLaunchTarget_HostPrefixNotFound(t *testing.T) {
	// Not parallel: t.Setenv empties PATH.
	t.Setenv("PATH", emptyDir(t))
	for _, carrier := range []domain.LaunchCarrier{nil, &agenttest.FakeCarrier{}} {
		_, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: "opencode acp"},
			HostPrefix:    []string{"acp-broker", "--"},
			Carrier:       carrier,
		}, "")
		if agentErr == nil || agentErr.Kind != domain.ErrAgentNotFound || !strings.Contains(agentErr.Message, "acp-broker") {
			t.Errorf("ResolveLaunchTarget(carrier %v) error = %v, want an agent_not_found error naming the prefix program", carrier != nil, agentErr)
		}
	}
}

func TestSessionLaunch_HostPrefixPrecedesCarrier(t *testing.T) {
	// Not parallel: brokerOnPath sets PATH.
	broker := brokerOnPath(t)
	ws := t.TempDir()
	carrier := &agenttest.FakeCarrier{}
	params := domain.StartSessionParams{
		WorkspacePath: ws,
		AgentConfig:   domain.AgentConfig{Command: "opencode acp"},
		SessionEnv:    []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "1"}},
		Carrier:       carrier,
	}
	bare, agentErr := ResolveLaunchTarget(params, "")
	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
	}
	params.HostPrefix = []string{"acp-broker", "-http", "127.0.0.1:7701", "--"}
	target, agentErr := ResolveLaunchTarget(params, "")
	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget(host prefix) error = %v", agentErr)
	}

	carried, agentErr := bare.SessionLaunch([]string{"--pure"})
	if agentErr != nil {
		t.Fatalf("SessionLaunch() error = %v", agentErr)
	}
	got, agentErr := target.SessionLaunch([]string{"--pure"})
	if agentErr != nil {
		t.Fatalf("SessionLaunch(host prefix) error = %v", agentErr)
	}
	want := append([]string{"-http", "127.0.0.1:7701", "--", carried.Path}, carried.Args...)
	if got.Path != broker || !slices.Equal(got.Args, want) {
		t.Errorf("SessionLaunch(host prefix) = %q %q, want %q %q: the prefix, then the carrier's launch", got.Path, got.Args, broker, want)
	}
	if l := carrier.Launches(); len(l) != 2 || !slices.Equal(l[1].Argv, []string{"opencode", "acp", "--pure"}) {
		t.Errorf("carrier launches = %+v, want the agent argv carried without the prefix", l)
	}
}

func TestSessionLaunch_HostPrefixPrecedesSSH(t *testing.T) {
	// Not parallel: t.Setenv puts a stand-in ssh beside the prefix.
	dir := t.TempDir()
	broker := agenttest.WriteScript(t, dir, "acp-broker", "exec \"$@\"\n")
	ssh := agenttest.WriteScript(t, dir, "ssh", "exit 0\n")
	t.Setenv("PATH", dir)

	target, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "opencode acp"},
		SSHHost:       "worker",
		SessionEnv:    []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "1"}},
		HostPrefix:    []string{"acp-broker", "--"},
	}, "")
	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
	}
	remote, agentErr := target.RemoteLaunch([]string{"--pure"})
	if agentErr != nil {
		t.Fatalf("RemoteLaunch() error = %v", agentErr)
	}
	got, agentErr := target.SessionLaunch([]string{"--pure"})
	if agentErr != nil {
		t.Fatalf("SessionLaunch() error = %v", agentErr)
	}
	want := append([]string{"--", ssh}, remote.Args...)
	if got.Path != broker || !slices.Equal(got.Args, want) {
		t.Errorf("SessionLaunch() = %q %q, want %q %q: the prefix, then the ssh launch", got.Path, got.Args, broker, want)
	}
	gotStdin, _ := io.ReadAll(got.StdinReader())
	wantStdin, _ := io.ReadAll(remote.StdinReader())
	if len(gotStdin) == 0 || !bytes.Equal(gotStdin, wantStdin) {
		t.Errorf("StdinReader() = %d bytes, want the ssh launch's %d-byte preamble", len(gotStdin), len(wantStdin))
	}
}

func TestAuxiliaryCommand_NeverRunsBehindHostPrefix(t *testing.T) {
	// Not parallel: brokerOnPath sets PATH.
	sshDir := t.TempDir()
	agenttest.WriteScript(t, sshDir, "ssh", "exit 0\n")
	broker := brokerOnPath(t)
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+sshDir)
	prefix := []string{"acp-broker", "--"}

	tests := []struct {
		name   string
		params domain.StartSessionParams
	}{
		{name: "local", params: domain.StartSessionParams{}},
		{name: "ssh", params: domain.StartSessionParams{SSHHost: "worker"}},
		{name: "carrier", params: domain.StartSessionParams{Carrier: &agenttest.FakeCarrier{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := tt.params
			params.WorkspacePath = t.TempDir()
			params.AgentConfig = domain.AgentConfig{Command: "opencode acp"}
			params.HostPrefix = prefix
			target, agentErr := ResolveLaunchTarget(params, "")
			if agentErr != nil {
				t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
			}
			cmd, agentErr := target.AuxiliaryCommand(context.Background(), []string{"--version"}, nil, nil)
			if agentErr != nil {
				t.Fatalf("AuxiliaryCommand() error = %v", agentErr)
			}
			if cmd.Path == broker || slices.Contains(cmd.Args, broker) || slices.Contains(cmd.Args, "acp-broker") {
				t.Errorf("AuxiliaryCommand() = %q %q, want no host prefix", cmd.Path, cmd.Args)
			}

			session, agentErr := target.SessionLaunch([]string{"--version"})
			if agentErr != nil {
				t.Fatalf("SessionLaunch() error = %v", agentErr)
			}
			if session.Path != broker {
				t.Errorf("SessionLaunch() path = %q, want the prefix %q: only the session runs behind it", session.Path, broker)
			}
		})
	}
	t.Run("local auxiliary command is the agent's own", func(t *testing.T) {
		target, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: "opencode acp"},
			HostPrefix:    prefix,
		}, "")
		if agentErr != nil {
			t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
		}
		cmd, agentErr := target.AuxiliaryCommand(context.Background(), []string{"--version"}, nil, nil)
		if agentErr != nil {
			t.Fatalf("AuxiliaryCommand() error = %v", agentErr)
		}
		if want := []string{"opencode", "acp", "--version"}; !slices.Equal(cmd.Args, want) {
			t.Errorf("AuxiliaryCommand() args = %q, want %q", cmd.Args, want)
		}
	})
}

// guestEnvCarrier stands in for an environment that runs the agent
// elsewhere: its launch clears the environment and sets only what
// [environment.GuestEnv] lets cross, as the guest of such an
// environment receives.
type guestEnvCarrier struct{}

func (guestEnvCarrier) LaunchArgv(launch domain.CarrierLaunch) ([]string, error) {
	argv := []string{"/usr/bin/env", "-i"}
	argv = append(argv, environment.GuestEnv(launch.Env, "/workspace", "stand-in")...)
	argv = append(argv, launch.AgentEnv...)
	return append(argv, launch.Argv...), nil
}

func (guestEnvCarrier) GuestPath(hostPath string) string { return hostPath }

func (guestEnvCarrier) ConnectionFailed(int, []string) bool { return false }

func TestSessionLaunch_HostPrefixKeepsTheSessionTokenOnTheHost(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skipf("skipping: /usr/bin/env: %v", err)
	}

	const token = "sortie-host-prefix-token"
	const apiURL = "http://127.0.0.1:7678"
	prefixPath, runs := agenttest.WriteHostPrefix(t, t.TempDir(), "broker", "SORTIE_SESSION_TOKEN", "SORTIE_API_URL")
	target, agentErr := ResolveLaunchTarget(domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig: domain.AgentConfig{CommandArgv: []string{
			"/bin/sh", "-c", `printf '%s|%s|%s' "${SORTIE_SESSION_TOKEN-unset}" "${SORTIE_API_URL-unset}" "$SORTIE_ISSUE_ID"`,
		}},
		SessionEnv: []domain.EnvVar{
			{Name: "SORTIE_ISSUE_ID", Value: "10001"},
			{Name: "SORTIE_SESSION_TOKEN", Value: token},
			{Name: "SORTIE_API_URL", Value: apiURL},
		},
		Carrier:    guestEnvCarrier{},
		HostPrefix: []string{prefixPath, "--"},
	}, "")
	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
	}
	launch, agentErr := target.SessionLaunch(nil)
	if agentErr != nil {
		t.Fatalf("SessionLaunch() error = %v", agentErr)
	}
	cmd := exec.CommandContext(context.Background(), launch.Path, launch.Args...)
	cmd.Env = os.Environ()
	if bindErr := target.BindWorkspace(cmd); bindErr != nil {
		t.Fatalf("BindWorkspace() error = %v", bindErr)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("session launch: %v", err)
	}

	got := runs()
	if len(got) != 1 {
		t.Fatalf("host prefix runs = %d, want 1", len(got))
	}
	run := got[0]
	if run.Env["SORTIE_SESSION_TOKEN"] != token || run.Env["SORTIE_API_URL"] != apiURL {
		t.Errorf("host prefix env = %q, want the session token and API URL", run.Env)
	}
	if len(run.Args) < 2 || run.Args[0] != "--" || run.Args[1] != "/usr/bin/env" {
		t.Fatalf("host prefix args = %q, want the rest of the prefix, then the carrier's launch", run.Args)
	}
	for _, arg := range run.Args[1:] {
		if strings.Contains(arg, "SORTIE_SESSION_TOKEN=") || strings.Contains(arg, token) || strings.Contains(arg, "SORTIE_API_URL=") || strings.Contains(arg, apiURL) {
			t.Errorf("carrier argv %q carries %q, want neither the session token nor the API URL", run.Args[1:], arg)
		}
	}
	if want := "unset|unset|10001"; string(out) != want {
		t.Errorf("agent saw %q, want %q: the session identity, without the token or the API URL", out, want)
	}
	if filepath.Base(run.Program) != "broker" {
		t.Errorf("host prefix program = %q, want the prefix itself", run.Program)
	}
}
