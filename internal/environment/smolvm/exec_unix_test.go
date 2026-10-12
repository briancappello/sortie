//go:build unix

package smolvm

import (
	"bytes"
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspace"
)

const (
	testSessionToken = "sortie-session-token-value"
	testAPIURL       = "http://127.0.0.1:7777/api"
)

// sessionEnv is a command environment as Sortie builds one on the host:
// infrastructure variables, the session identity, the session token and
// API URL, and the host workspace path.
func sessionEnv(hostPath string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/operator",
		"OPENROUTER_API_KEY=not-for-the-guest",
		"SORTIE_ISSUE_ID=10001",
		"SORTIE_ISSUE_IDENTIFIER=VM-1",
		"SORTIE_SESSION_TOKEN=" + testSessionToken,
		"SORTIE_API_URL=" + testAPIURL,
		"SORTIE_WORKSPACE=" + hostPath,
		"SORTIE_ATTEMPT=2",
	}
}

func assertNoSessionSecret(t *testing.T, argv []string) {
	t.Helper()
	joined := strings.Join(argv, "\x00")
	for _, forbidden := range []string{testSessionToken, testAPIURL, "SORTIE_SESSION_TOKEN", "SORTIE_API_URL", "not-for-the-guest", "OPENROUTER_API_KEY"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("argv %q contains %q", argv, forbidden)
		}
	}
}

// Not parallel: it sets process environment variables pass_env reads.
func TestCommand_Argv(t *testing.T) {
	t.Setenv("SORTIE_SMOLVM_TEST_PASS_SET", "plain value")
	t.Setenv("SORTIE_SMOLVM_TEST_PASS_EMPTY", "")
	t.Setenv("SORTIE_SESSION_TOKEN", testSessionToken)
	t.Setenv("SORTIE_API_URL", testAPIURL)
	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.PassEnv = []string{
			"SORTIE_SMOLVM_TEST_PASS_SET",
			"SORTIE_SMOLVM_TEST_PASS_UNSET",
			"SORTIE_SMOLVM_TEST_PASS_EMPTY",
			"SORTIE_SMOLVM_TEST_PASS_SET",
			"SORTIE_SESSION_TOKEN",
			"SORTIE_API_URL",
			"SORTIE_ISSUE_ID",
		}
	})

	tests := []struct {
		name string
		spec environment.CommandSpec
		want []string
	}{
		{
			name: "root without stdin streams output",
			spec: environment.CommandSpec{Argv: []string{"sh", "-c", "uname -n > created-on"}, Env: sessionEnv(f.ws.HostPath)},
			want: []string{"machine", "exec", "--name", "sortie-VM-1", "--stream", "-w", "/workspace"},
		},
		{
			name: "subdirectory with stdin is interactive",
			spec: environment.CommandSpec{Argv: []string{"git", "hash-object", "--stdin-paths"}, Dir: "sub/dir", Env: sessionEnv(f.ws.HostPath), Stdin: true},
			want: []string{"machine", "exec", "--name", "sortie-VM-1", "-i", "-w", "/workspace/sub/dir"},
		},
	}
	for _, tt := range tests {
		cmd, err := f.env.Command(context.Background(), f.ws, tt.spec)
		if err != nil {
			t.Fatalf("%s: Command() error = %v", tt.name, err)
		}
		want := append(slices.Clone(tt.want),
			"-e", "SORTIE_ISSUE_ID=10001",
			"-e", "SORTIE_ISSUE_IDENTIFIER=VM-1",
			"-e", "SORTIE_ATTEMPT=2",
			"-e", "SORTIE_WORKSPACE=/workspace",
			"-e", "SORTIE_ENVIRONMENT=smolvm",
			"--secret-env", "SORTIE_SMOLVM_TEST_PASS_SET=SORTIE_SMOLVM_TEST_PASS_SET",
			"--secret-env", "SORTIE_SMOLVM_TEST_PASS_EMPTY=SORTIE_SMOLVM_TEST_PASS_EMPTY",
			"--")
		want = append(want, tt.spec.Argv...)
		if cmd.Path != f.fake.Binary || !slices.Equal(cmd.Args[1:], want) {
			t.Errorf("%s: Command() = %s %q\nwant %s %q", tt.name, cmd.Path, cmd.Args[1:], f.fake.Binary, want)
		}
		if cmd.Env != nil || cmd.Dir != "" {
			t.Errorf("%s: Command() Env = %q, Dir = %q, want Sortie's own environment and no host directory", tt.name, cmd.Env, cmd.Dir)
		}
		assertNoSessionSecret(t, cmd.Args)
		if strings.Contains(strings.Join(cmd.Args, " "), "plain value") {
			t.Errorf("%s: argv %q carries a pass_env value, want only its name", tt.name, cmd.Args)
		}
	}
}

func TestCommand_RunsThroughSmolvm(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{managedVM1(StateRunning)}}, nil)
	cmd, err := f.env.Command(context.Background(), f.ws, environment.CommandSpec{Argv: []string{"uname", "-n"}})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "uname -n" {
		t.Errorf("Command().Output() = %q, %v, want the guest command's output", out, err)
	}
}

func TestCommand_Refusals(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	noKey := f.ws
	noKey.Key = ""
	for name, tc := range map[string]struct {
		ws   environment.Workspace
		spec environment.CommandSpec
	}{
		"empty argv":       {f.ws, environment.CommandSpec{}},
		"parent directory": {f.ws, environment.CommandSpec{Argv: []string{"true"}, Dir: "../other"}},
		"absolute dir":     {f.ws, environment.CommandSpec{Argv: []string{"true"}, Dir: "/etc"}},
		"inner escape":     {f.ws, environment.CommandSpec{Argv: []string{"true"}, Dir: "a/../../b"}},
		"no workspace key": {noKey, environment.CommandSpec{Argv: []string{"true"}}},
	} {
		if cmd, err := f.env.Command(context.Background(), tc.ws, tc.spec); err == nil || cmd != nil {
			t.Errorf("%s: Command() = %v, %v, want a refusal", name, cmd, err)
		}
	}
	if got := f.fake.argvs(); len(got) != 0 {
		t.Errorf("calls = %q, want none", got)
	}
}

// Not parallel: it sets a process environment variable pass_env reads.
func TestLaunchArgv(t *testing.T) {
	t.Setenv("SORTIE_SMOLVM_TEST_PASS_SET", "plain value")
	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.PassEnv = []string{"SORTIE_SMOLVM_TEST_PASS_SET", "SORTIE_SMOLVM_TEST_PASS_UNSET"}
	})
	carrier := environment.LaunchCarrier(f.env, f.ws)
	if carrier == nil {
		t.Fatal("LaunchCarrier() = nil, want the smolvm carrier")
	}
	launch := domain.CarrierLaunch{
		Argv:     []string{"opencode", "acp"},
		Env:      sessionEnv(f.ws.HostPath),
		AgentEnv: []string{"OPENCODE_CONFIG_CONTENT={\"a\": 1}"},
	}
	head := []string{
		f.fake.Binary, "machine", "exec", "--name", "sortie-VM-1", "-i", "-w", "/workspace",
		"-e", "SORTIE_ISSUE_ID=10001",
		"-e", "SORTIE_ISSUE_IDENTIFIER=VM-1",
		"-e", "SORTIE_ATTEMPT=2",
		"-e", "SORTIE_WORKSPACE=/workspace",
		"-e", "SORTIE_ENVIRONMENT=smolvm",
		"-e", "OPENCODE_CONFIG_CONTENT={\"a\": 1}",
		"--secret-env", "SORTIE_SMOLVM_TEST_PASS_SET=SORTIE_SMOLVM_TEST_PASS_SET",
		"--",
	}

	argv, err := carrier.LaunchArgv(launch)
	if want := append(slices.Clone(head), "opencode", "acp"); err != nil || !slices.Equal(argv, want) {
		t.Errorf("LaunchArgv() = %q, %v\nwant %q", argv, err, want)
	}
	assertNoSessionSecret(t, argv)

	launch.Unset = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
	argv, err = carrier.LaunchArgv(launch)
	want := append(slices.Clone(head), "env", "-u", "ANTHROPIC_API_KEY", "-u", "OPENAI_API_KEY", "--", "opencode", "acp")
	if err != nil || !slices.Equal(argv, want) {
		t.Errorf("LaunchArgv() with Unset = %q, %v\nwant %q", argv, err, want)
	}

	launch.Unset = []string{"A=B"}
	if _, err := carrier.LaunchArgv(launch); err == nil {
		t.Error("LaunchArgv() with an Unset entry holding '=' error = nil, want a refusal")
	}
	if _, err := carrier.LaunchArgv(domain.CarrierLaunch{}); err == nil {
		t.Error("LaunchArgv() without an agent command error = nil, want a refusal")
	}
	if got := carrier.GuestPath(f.ws.HostPath + "/.sortie/mcp.json"); got != "/workspace/.sortie/mcp.json" {
		t.Errorf("GuestPath() = %q, want /workspace/.sortie/mcp.json", got)
	}
}

func TestConnectionFailed(t *testing.T) {
	t.Parallel()

	env := &Environment{}
	ws := environment.Workspace{Key: "VM-1"}
	usage := []string{
		"error: unexpected argument '--bogus' found",
		"",
		"  tip: to pass '--bogus' as a value, use '-- --bogus'",
		"",
		"Usage: smolvm machine exec --name <NAME> <COMMAND>...",
		"",
		"For more information, try '--help'.",
	}
	tests := []struct {
		name string
		exit int
		tail []string
		want bool
	}{
		// smolvm 1.26.1's messages, measured.
		{"vm not found", 1, []string{"Error: vm not found: sortie-VM-1"}, true},
		{"vm not running", 1, []string{"Error: agent operation failed: connect: machine 'sortie-VM-1' is not running. Use 'smolvm machine start --name sortie-VM-1' first."}, true},
		{"vm lost while running", 1, []string{"started", "Error: io operation failed: connection closed"}, true},
		{"secret not resolved", 1, []string{"Error: config operation failed: resolve secret 'FOO': env_unset"}, true},
		{"usage error", 2, usage, true},
		{"trailing blank lines", 1, []string{"Error: vm not found: sortie-VM-1", "", "  "}, true},

		{"guest exit 1", 1, []string{"guest-err"}, false},
		{"guest exit 1 without output", 1, nil, false},
		{"guest exit 7", 7, []string{"Error: vm not found: sortie-VM-1"}, false},
		{"missing guest program", 255, []string{"executable file `opencode` not found in $PATH: No such file or directory"}, false},
		{"missing guest directory", 255, []string{"chdir `/workspace/x`: No such file or directory"}, false},
		{"another VM", 1, []string{"Error: vm not found: sortie-VM-2"}, false},
		{"another VM not running", 1, []string{"Error: agent operation failed: connect: machine 'sortie-VM-10' is not running."}, false},
		{"smolvm line followed by guest output", 1, []string{"Error: vm not found: sortie-VM-1", "more guest output"}, false},
		{"guest clap usage error", 2, []string{"Usage: opencode acp [OPTIONS]", "", "For more information, try '--help'."}, false},
		{"killed", -1, []string{"Error: vm not found: sortie-VM-1"}, false},
	}
	for _, tt := range tests {
		if got := env.ConnectionFailed(ws, tt.exit, tt.tail); got != tt.want {
			t.Errorf("%s: ConnectionFailed(%d, %q) = %v, want %v", tt.name, tt.exit, tt.tail, got, tt.want)
		}
	}
	if env.ConnectionFailed(environment.Workspace{}, 1, []string{"Error: vm not found: sortie-"}) {
		t.Error("ConnectionFailed() without a workspace key = true, want false")
	}
}

func TestConnectionFailed_ClassifiesSmolvmsOwnExit(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{managedVM1(StateStopped)}}, nil)
	for name, ws := range map[string]environment.Workspace{
		"stopped VM": f.ws,
		"missing VM": {Key: "VM-2", HostPath: f.ws.HostPath, GuestPath: GuestWorkspace},
	} {
		argv, err := f.env.LaunchArgv(ws, domain.CarrierLaunch{Argv: []string{"opencode", "acp"}})
		if err != nil {
			t.Fatalf("%s: LaunchArgv() error = %v", name, err)
		}
		cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: the fake smolvm
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		exit := procutil.ExtractExitCode(runErr)
		if tail := strings.Split(stderr.String(), "\n"); !f.env.ConnectionFailed(ws, exit, tail) {
			t.Errorf("%s: ConnectionFailed(%d, %q) = false, want true", name, exit, tail)
		}
	}
}

func TestRunHook_ReachesTheGuestWithTheEnvironmentKind(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{managedVM1(StateRunning)}}, nil)
	target := workspace.NewTarget(f.env, f.ws.Key, f.ws.HostPath, "10001", "VM-1")
	res, err := workspace.RunHook(context.Background(), workspace.HookParams{
		Script:    "uname -n > created-on",
		Dir:       f.ws.HostPath,
		Env:       workspace.HookEnv("10001", "VM-1", f.ws.HostPath, 1),
		TimeoutMS: 30_000,
		Target:    target,
	})
	if err != nil {
		t.Fatalf("RunHook() error = %v", err)
	}
	if !strings.Contains(res.Output, "sh -c uname -n > created-on") {
		t.Errorf("RunHook() output = %q, want the guest command's", res.Output)
	}
	calls := f.fake.argvs()
	if len(calls) != 1 {
		t.Fatalf("calls = %q, want one exec", calls)
	}
	execLine := strings.Join(calls[0], " ")
	for _, want := range []string{"--name sortie-VM-1 --stream -w /workspace ", "-e SORTIE_ENVIRONMENT=smolvm", "-e SORTIE_WORKSPACE=/workspace", "-e SORTIE_ISSUE_IDENTIFIER=VM-1", "-- sh -c uname -n > created-on"} {
		if !strings.Contains(execLine, want) {
			t.Errorf("hook exec %q does not contain %q", execLine, want)
		}
	}
	if strings.Contains(execLine, f.ws.HostPath) {
		t.Errorf("hook exec %q names the host workspace", execLine)
	}
}
