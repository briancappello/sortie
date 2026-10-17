package agentcore

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

func sessionEnvFixture() []domain.EnvVar {
	return []domain.EnvVar{
		{Name: "SORTIE_ISSUE_ID", Value: "42"},
		{Name: "SORTIE_ISSUE_IDENTIFIER", Value: "LIVE-1"},
		{Name: "SORTIE_ATTEMPT", Value: ""},
		{Name: "SORTIE_SESSION_TOKEN", Value: "session-token-value-0123456789"},
	}
}

func TestResolveLaunchTarget_CarriesSessionEnv(t *testing.T) {
	// Not parallel: the SSH-mode subtest uses t.Setenv.
	dir := t.TempDir()
	binPath := agenttest.FakeRuntime(t, t.TempDir(), "agent", agenttest.OutputScenario, agenttest.Output{})

	t.Run("local string command", func(t *testing.T) {
		params := makeParams(t, dir, "", binPath)
		params.SessionEnv = sessionEnvFixture()
		lt, agentErr := ResolveLaunchTarget(params, binPath)
		if agentErr != nil {
			t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
		}
		if !slices.Equal(lt.SessionEnv, params.SessionEnv) {
			t.Errorf("LaunchTarget.SessionEnv = %v, want %v", lt.SessionEnv, params.SessionEnv)
		}
	})

	t.Run("local argv command", func(t *testing.T) {
		params := makeParams(t, dir, "", "")
		params.AgentConfig.CommandArgv = []string{binPath}
		params.SessionEnv = sessionEnvFixture()
		lt, agentErr := ResolveLaunchTarget(params, "")
		if agentErr != nil {
			t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
		}
		if !slices.Equal(lt.SessionEnv, params.SessionEnv) {
			t.Errorf("LaunchTarget.SessionEnv = %v, want %v", lt.SessionEnv, params.SessionEnv)
		}
	})

	t.Run("ssh mode", func(t *testing.T) {
		t.Setenv("PATH", fakeSSHDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
		params := makeParams(t, dir, "user@host", "agent")
		params.SessionEnv = sessionEnvFixture()
		lt, agentErr := ResolveLaunchTarget(params, "agent")
		if agentErr != nil {
			t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
		}
		if !slices.Equal(lt.SessionEnv, params.SessionEnv) {
			t.Errorf("LaunchTarget.SessionEnv = %v, want %v", lt.SessionEnv, params.SessionEnv)
		}
	})
}

func TestLaunchTarget_BindWorkspace_SessionEnv(t *testing.T) {
	t.Parallel()

	t.Run("session values are set last and override inherited ones", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		target := LaunchTarget{WorkspacePath: ws, SessionEnv: sessionEnvFixture()}
		cmd := exec.Command("true")
		cmd.Env = []string{"FOO=bar", "SORTIE_ISSUE_ID=old", "SORTIE_ATTEMPT=7"}

		if agentErr := target.BindWorkspace(cmd); agentErr != nil {
			t.Fatalf("BindWorkspace() error = %v", agentErr)
		}

		want := []string{
			"FOO=bar", "PWD=" + ws,
			"SORTIE_ISSUE_ID=42", "SORTIE_ISSUE_IDENTIFIER=LIVE-1", "SORTIE_ATTEMPT=",
			"SORTIE_SESSION_TOKEN=session-token-value-0123456789",
		}
		if !slices.Equal(cmd.Env, want) {
			t.Errorf("BindWorkspace().Env = %v, want %v", cmd.Env, want)
		}
	})

	t.Run("a session name that is also withheld is still set", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		target := LaunchTarget{
			WorkspacePath: ws,
			WithheldEnv:   []string{"SORTIE_ISSUE_ID"},
			SessionEnv:    []domain.EnvVar{{Name: "SORTIE_ISSUE_ID", Value: "42"}},
		}
		cmd := exec.Command("true")
		cmd.Env = []string{"SORTIE_ISSUE_ID=old"}

		if agentErr := target.BindWorkspace(cmd); agentErr != nil {
			t.Fatalf("BindWorkspace() error = %v", agentErr)
		}
		if want := []string{"PWD=" + ws, "SORTIE_ISSUE_ID=42"}; !slices.Equal(cmd.Env, want) {
			t.Errorf("BindWorkspace().Env = %v, want %v", cmd.Env, want)
		}
	})

	t.Run("no session env leaves the environment as before", func(t *testing.T) {
		t.Parallel()
		ws := t.TempDir()
		target := LaunchTarget{WorkspacePath: ws}
		cmd := exec.Command("true")
		cmd.Env = []string{"SORTIE_ISSUE_ID=old"}

		if agentErr := target.BindWorkspace(cmd); agentErr != nil {
			t.Fatalf("BindWorkspace() error = %v", agentErr)
		}
		if want := []string{"SORTIE_ISSUE_ID=old", "PWD=" + ws}; !slices.Equal(cmd.Env, want) {
			t.Errorf("BindWorkspace().Env = %v, want %v", cmd.Env, want)
		}
	})
}

// TestLaunchTarget_BindWorkspace_SessionEnvReachesProcess runs a real
// child with an inherited stale value and asserts the child reads the
// session value.
func TestLaunchTarget_BindWorkspace_SessionEnvReachesProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	t.Setenv("SORTIE_ISSUE_ID", "old")

	ws := t.TempDir()
	target := LaunchTarget{WorkspacePath: ws, SessionEnv: sessionEnvFixture()}
	cmd := exec.Command("/bin/sh", "-c", `printf '%s|%s|%s' "$SORTIE_ISSUE_ID" "$SORTIE_ISSUE_IDENTIFIER" "${SORTIE_ATTEMPT-unset}"`)
	if agentErr := target.BindWorkspace(cmd); agentErr != nil {
		t.Fatalf("BindWorkspace() error = %v", agentErr)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := out.String(), "42|LIVE-1|"; got != want {
		t.Errorf("child read %q, want %q", got, want)
	}
}

func TestLaunchTarget_SSHOptions_SessionEnv(t *testing.T) {
	// Not parallel: sets process environment via t.Setenv.
	t.Setenv("SORTIE_ISSUE_ID", "orchestrator-value")
	t.Setenv("SSH_SESSIONENV_TEST_KEPT", "kept")

	target := LaunchTarget{
		SSHEnvNames: []string{"SSH_SESSIONENV_TEST_KEPT", "SORTIE_ISSUE_ID"},
		WithheldEnv: []string{"SORTIE_ISSUE_IDENTIFIER", "SSH_SESSIONENV_TEST_HELD"},
		SessionEnv:  sessionEnvFixture(),
	}
	settings := []sshutil.EnvVar{{Name: "SORTIE_ATTEMPT", Value: "setting-value"}, {Name: "SSH_SESSIONENV_TEST_SETTING", Value: "s"}}

	got := target.SSHOptions(settings...)

	want := []sshutil.EnvVar{
		{Name: "SSH_SESSIONENV_TEST_KEPT", Value: "kept"},
		{Name: "SSH_SESSIONENV_TEST_SETTING", Value: "s"},
		{Name: "SORTIE_ISSUE_ID", Value: "42"},
		{Name: "SORTIE_ISSUE_IDENTIFIER", Value: "LIVE-1"},
		{Name: "SORTIE_ATTEMPT", Value: ""},
		{Name: "SORTIE_SESSION_TOKEN", Value: "session-token-value-0123456789"},
	}
	if !slices.Equal(got.Env, want) {
		t.Errorf("SSHOptions().Env = %#v, want %#v", envPairs(got.Env), envPairs(want))
	}
	if wantUnset := []string{"SSH_SESSIONENV_TEST_HELD"}; !slices.Equal(got.Unset, wantUnset) {
		t.Errorf("SSHOptions().Unset = %v, want %v", got.Unset, wantUnset)
	}
}

// TestLaunchTarget_SSHLaunch_SessionEnvOnStdinOnly asserts that a remote
// launch carries every session value on standard input and never in the
// SSH argument list.
func TestLaunchTarget_SSHLaunch_SessionEnvOnStdinOnly(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	target := LaunchTarget{
		Command:       "/usr/bin/ssh",
		WorkspacePath: ws,
		RemoteCommand: "agent",
		SSHHost:       "worker-host",
		SessionEnv:    sessionEnvFixture(),
	}

	cmd, agentErr := target.AuxiliaryCommand(context.Background(), []string{"run"}, strings.NewReader(""), nil)
	if agentErr != nil {
		t.Fatalf("AuxiliaryCommand() error = %v", agentErr)
	}
	stdin, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("io.ReadAll(cmd.Stdin): %v", err)
	}
	args := strings.Join(cmd.Args, "\x00")
	for _, v := range []string{"LIVE-1", "session-token-value-0123456789"} {
		if strings.Contains(args, v) {
			t.Errorf("ssh arguments carry session value %q: %q", v, cmd.Args)
		}
		if !strings.Contains(string(stdin), v) {
			t.Errorf("ssh standard input = %q, want it to carry %q", stdin, v)
		}
	}
	if !strings.Contains(string(stdin), "SORTIE_ATTEMPT=''") {
		t.Errorf("ssh standard input = %q, want SORTIE_ATTEMPT exported as the empty string", stdin)
	}
}

func envPairs(env []sshutil.EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Name+"="+e.Value)
	}
	return out
}
