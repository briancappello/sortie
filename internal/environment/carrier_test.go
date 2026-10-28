package environment_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/environment/environmenttest"
)

func TestGuestEnv(t *testing.T) {
	t.Parallel()

	env := []string{
		"PATH=/usr/bin",
		"SORTIE_ISSUE_ID=10001",
		"SORTIE_SESSION_TOKEN=secret-token",
		"SORTIE_API_URL=http://127.0.0.1:7678",
		"SORTIE_WORKSPACE=/host/root/ENV-1",
		"SORTIE_ENVIRONMENT=local",
		"OPENROUTER_API_KEY=sk-secret",
		"SORTIE_ATTEMPT=2",
		"SORTIE_ISSUE_ID=shadowed",
		"MALFORMED",
	}
	got := environment.GuestEnv(env, "/workspace", "vmkind")
	want := []string{
		"SORTIE_ISSUE_ID=10001",
		"SORTIE_ATTEMPT=2",
		"SORTIE_WORKSPACE=/workspace",
		"SORTIE_ENVIRONMENT=vmkind",
	}
	if !slices.Equal(got, want) {
		t.Errorf("GuestEnv() = %q, want %q: only SORTIE_* identity crosses, never the session token or API URL", got, want)
	}
}

func TestWorkspaceGuestPathOf(t *testing.T) {
	t.Parallel()

	host := filepath.Join(t.TempDir(), "ENV-1")
	ws := environment.Workspace{HostPath: host, GuestPath: "/workspace"}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"workspace root", host, "/workspace"},
		{"control file", filepath.Join(host, ".sortie", "mcp.json"), "/workspace/.sortie/mcp.json"},
		{"outside the workspace", filepath.Join(filepath.Dir(host), "other", "mcp.json"), filepath.Join(filepath.Dir(host), "other", "mcp.json")},
		{"sibling with the same prefix", host + "-2/mcp.json", host + "-2/mcp.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ws.GuestPathOf(tt.in); got != tt.want {
				t.Errorf("GuestPathOf(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// fakeCarrierEnvironment carries launches into a workspace it records.
type fakeCarrierEnvironment struct {
	environmenttest.Recorder
	gotWorkspace environment.Workspace
	gotLaunch    domain.CarrierLaunch
	gotExit      int
	gotTail      []string

	gotFailedWorkspace environment.Workspace
}

func (f *fakeCarrierEnvironment) LaunchArgv(ws environment.Workspace, launch domain.CarrierLaunch) ([]string, error) {
	f.gotWorkspace, f.gotLaunch = ws, launch
	return append([]string{"vm", "exec", "-w", ws.GuestPath, "--"}, launch.Argv...), nil
}

func (f *fakeCarrierEnvironment) ConnectionFailed(ws environment.Workspace, exitCode int, stderrTail []string) bool {
	f.gotFailedWorkspace, f.gotExit, f.gotTail = ws, exitCode, stderrTail
	return exitCode == 1 && len(stderrTail) > 0
}

func TestLaunchCarrier(t *testing.T) {
	t.Parallel()

	local, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: config.EnvironmentKindLocal}})
	if err != nil {
		t.Fatalf("New(local) error = %v", err)
	}
	if c := environment.LaunchCarrier(local, environment.Workspace{HostPath: "/h", GuestPath: "/h"}); c != nil {
		t.Errorf("LaunchCarrier(local) = %v, want nil: local launches the agent as it does without an environment", c)
	}

	env := &fakeCarrierEnvironment{}
	ws := environment.Workspace{Key: "ENV-1", HostPath: "/root/ENV-1", GuestPath: "/workspace", IssueID: "1", Identifier: "ENV-1"}
	c := environment.LaunchCarrier(env, ws)
	if c == nil {
		t.Fatal("LaunchCarrier(carrier) = nil, want a carrier bound to the workspace")
	}
	launch := domain.CarrierLaunch{Argv: []string{"opencode", "acp"}, Env: []string{"SORTIE_ISSUE_ID=1"}}
	argv, err := c.LaunchArgv(launch)
	if err != nil || !slices.Equal(argv, []string{"vm", "exec", "-w", "/workspace", "--", "opencode", "acp"}) {
		t.Errorf("LaunchArgv() = %q, %v", argv, err)
	}
	if env.gotWorkspace != ws {
		t.Errorf("carrier workspace = %+v, want %+v", env.gotWorkspace, ws)
	}
	if got := c.Workspace(); got != "/workspace" {
		t.Errorf("Workspace() = %q, want /workspace", got)
	}
	if _, ok := c.ToolChannel(); ok {
		t.Error("ToolChannel() ok = true for an environment that offers no tool channel, want false")
	}
	if !c.ConnectionFailed(1, []string{"Error: vm not found"}) || env.gotExit != 1 || len(env.gotTail) != 1 || env.gotFailedWorkspace != ws {
		t.Errorf("ConnectionFailed() did not reach the environment with its workspace, exit code, and stderr tail: got %+v, %d, %q", env.gotFailedWorkspace, env.gotExit, env.gotTail)
	}
}

func TestRunsElsewhere(t *testing.T) {
	t.Parallel()

	local, err := environment.New(environment.Params{Config: config.EnvironmentConfig{Kind: config.EnvironmentKindLocal}})
	if err != nil {
		t.Fatalf("New(local) error = %v", err)
	}
	for name, tc := range map[string]struct {
		env  environment.Environment
		want bool
	}{
		"nil":      {nil, false},
		"local":    {local, false},
		"recorder": {&environmenttest.Recorder{}, false},
		"carrier":  {&fakeCarrierEnvironment{}, true},
	} {
		if got := environment.RunsElsewhere(tc.env); got != tc.want {
			t.Errorf("RunsElsewhere(%s) = %v, want %v", name, got, tc.want)
		}
	}
}
