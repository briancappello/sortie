//go:build unix

package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

const scenarioOpencodeSSHStandIn = "opencode.ssh-stand-in"

func init() {
	fakeScenarios[scenarioOpencodeSSHStandIn] = agenttest.Typed(runOpencodeSSHStandIn)
}

type sshStandInParams struct {
	PATH string
}

func runOpencodeSSHStandIn(args []string, params sshStandInParams) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ssh stand-in: no arguments")
		return 2
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh stand-in: sh not found: %v\n", err)
		return 2
	}

	// The dropped environment proves carried values reach the remote command
	// only through standard input.
	remoteCommand := args[len(args)-1]
	cmd := exec.Command(shPath, "-c", remoteCommand) //nolint:gosec // remoteCommand is the launch this test built
	cmd.Env = []string{"PATH=" + params.PATH}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "ssh stand-in: %v\n", err)
		return 2
	}
	return 0
}

func sshStandInEnvPath(t *testing.T, includeDD bool) string {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH: %v", err)
	}

	// A private directory: sh and dd usually share /usr/bin, so reusing sh's
	// own directory would resolve dd in the no-dd case too.
	dir := t.TempDir()
	if err := os.Symlink(shPath, filepath.Join(dir, "sh")); err != nil {
		t.Fatalf("Symlink(sh): %v", err)
	}

	if includeDD {
		ddPath, err := exec.LookPath("dd")
		if err != nil {
			t.Skipf("dd not found on PATH: %v", err)
		}
		if err := os.Symlink(ddPath, filepath.Join(dir, "dd")); err != nil {
			t.Fatalf("Symlink(dd): %v", err)
		}
	}
	return dir
}

func writeSSHCaptureScript(t *testing.T, dir, carriedName, envCapturePath, settingCapturePath string) string {
	t.Helper()
	content := "case \"$1\" in\n  --version) echo 'opencode v2.0.18'; exit 0;;\nesac\n" +
		"printf '%s' \"$" + carriedName + "\" > '" + envCapturePath + "'\n" +
		"printf '%s' \"$OPENCODE_DISABLE_AUTOUPDATE\" > '" + settingCapturePath + "'\n"
	return agenttest.WriteScript(t, dir, "fake-opencode-ssh", content)
}

func pollOpencodePIDAndAssertGone(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && v > 0 {
				pid = v
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("pid file %q never populated", path)
	}

	goneDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(goneDeadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant %d still answers signal 0, want it gone", pid)
}

func TestQueryExportUsage_HeldDescendantHoldingOutput(t *testing.T) {
	tmpDir := t.TempDir()
	fixturePath := filepath.Join(tmpDir, "export_usage.json")
	if err := os.WriteFile(fixturePath, loadFixture(t, "export_usage.json"), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
		t.Fatalf("WriteFile(export_usage.json): %v", err)
	}
	pidPath := filepath.Join(tmpDir, "descendant.pid")

	body := "cat '" + fixturePath + "'\n" +
		"sleep 30 & echo $! > '" + pidPath + "'\n"
	script := agenttest.WriteScript(t, tmpDir, "fake-export", body)
	state := testExportState(script, tmpDir)

	start := time.Now()
	usage := queryExportUsage(context.Background(), state, 0)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryExportUsage() took %v, want within 3s", elapsed)
	}
	if usage.Usage.InputTokens != 1750 {
		t.Errorf("InputTokens = %d, want 1750", usage.Usage.InputTokens)
	}
	if usage.Usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", usage.Usage.OutputTokens)
	}

	pollOpencodePIDAndAssertGone(t, pidPath)
}

func writeExportScript(t *testing.T, dir, fixtureName string, exitCode int) (string, string) {
	t.Helper()

	argsPath := filepath.Join(dir, "args.log")
	body := `printf '%s\n' "$@" > '` + argsPath + `'
exit ` + strconv.Itoa(exitCode)
	if fixtureName != "" && exitCode == 0 {
		fixturePath := filepath.Join(dir, fixtureName)
		if err := os.WriteFile(fixturePath, loadFixture(t, fixtureName), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", fixtureName, err)
		}
		body = `printf '%s\n' "$@" > '` + argsPath + `'
cat '` + fixturePath + `'`
	}

	return agenttest.WriteScript(t, dir, "fake-export", body), argsPath
}

func testExportState(command, workspace string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			Command:       command,
			WorkspacePath: workspace,
		},
		sessionID:  "ses_abc123",
		baseLogger: slog.Default(),
	}
}

func TestQueryExportSubprocess(t *testing.T) {
	t.Parallel()

	t.Run("the_warning_reports_an_export_that_recovered_nothing", func(t *testing.T) {
		t.Parallel()

		// A finished step whose provider reported zero is a measurement, so
		// saying no usage was found contradicts the verdict it produces. The
		// unfinished placeholder recovers nothing and must still say so.
		for name, tc := range map[string]struct {
			finish   string
			wantWarn bool
		}{
			"finished_zero_step": {finish: `"finish":"stop",`, wantWarn: false},
			"unfinished_step":    {finish: ``, wantWarn: true},
		} {
			tmpDir := t.TempDir()
			exportPath := filepath.Join(tmpDir, "export.json")
			export := `{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant",` + tc.finish +
				`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}]}`
			if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
				t.Fatal(err)
			}
			script := agenttest.WriteScript(t, tmpDir, "fake-export", "cat '"+exportPath+"'")

			var logs bytes.Buffer
			state := testExportState(script, tmpDir)
			state.baseLogger = slog.New(slog.NewTextHandler(&logs, nil))

			queryExportUsage(context.Background(), state, 0)

			if got := strings.Contains(logs.String(), "no assistant token usage found"); got != tc.wantWarn {
				t.Errorf("%s: warned = %v, want %v", name, got, tc.wantWarn)
			}
		}
	})

	t.Run("local_subprocess_usage_extracted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, argsPath := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.Usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.Usage.InputTokens)
		}
		if usage.Usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.Usage.OutputTokens)
		}
		if usage.Usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.Usage.CacheReadTokens)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("ReadFile(args.log): %v", err)
		}
		if string(args) != "session\nexport\n--standalone\n--sanitize\nses_abc123\n" {
			t.Errorf("export args = %q, want %q", string(args), "session\nexport\n--standalone\n--sanitize\nses_abc123\n")
		}
	})

	t.Run("local_subprocess_missing_tokens_returns_zero", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "export_usage_missing_tokens.json", 0)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if !reflect.DeepEqual(usage, exportUsage{}) {
			t.Errorf("usage = %+v, want zero value", usage)
		}
	})

	t.Run("local_subprocess_nonzero_exit_returns_zero", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "", 1)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if !reflect.DeepEqual(usage, exportUsage{}) {
			t.Errorf("usage = %+v, want zero value", usage)
		}
	})

	t.Run("ssh_subprocess_usage_extracted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, argsPath := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)
		state.target.RemoteCommand = "opencode"
		state.target.SSHHost = "example.test"

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.Usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.Usage.InputTokens)
		}
		if usage.Usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.Usage.OutputTokens)
		}
		if usage.Usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.Usage.CacheReadTokens)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("ReadFile(args.log): %v", err)
		}
		logged := string(args)
		if !strings.Contains(logged, "example.test") {
			t.Errorf("ssh args = %q, want host %q", logged, "example.test")
		}
		if !strings.Contains(logged, "export") || !strings.Contains(logged, "--sanitize") || !strings.Contains(logged, "ses_abc123") {
			t.Errorf("ssh args = %q, want export invocation details", logged)
		}
	})
}

func TestQueryExportUsage_SSH_CarriesEnvironmentVariableAndSetting(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	tmpDir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioOpencodeSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "OPENCODE_TEST_CARRY_EXPORT"
	const carriedValue = "carried-value-export"
	t.Setenv(carriedName, carriedValue)

	envCapturePath := filepath.Join(tmpDir, "env.out")
	settingCapturePath := filepath.Join(tmpDir, "setting.out")
	agentScript := writeSSHCaptureScript(t, tmpDir, carriedName, envCapturePath, settingCapturePath)

	state := testExportState(agentScript, tmpDir)
	state.target.RemoteCommand = agentScript
	state.target.SSHHost = "user@stand-in-host"
	state.target.SSHEnvNames = []string{carriedName}

	queryExportUsage(context.Background(), state, 0)

	gotEnv, err := os.ReadFile(envCapturePath)
	if err != nil {
		t.Fatalf("ReadFile(env.out): %v", err)
	}
	if string(gotEnv) != carriedValue {
		t.Errorf("carried variable = %q, want %q", string(gotEnv), carriedValue)
	}

	gotSetting, err := os.ReadFile(settingCapturePath)
	if err != nil {
		t.Fatalf("ReadFile(setting.out): %v", err)
	}
	if string(gotSetting) != "true" {
		t.Errorf("OPENCODE_DISABLE_AUTOUPDATE = %q, want %q", string(gotSetting), "true")
	}
}

func TestRunTurn_SSH_CarriesEnvironmentVariableAndSetting(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	tmpDir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioOpencodeSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "OPENCODE_TEST_CARRY_RUNTURN"
	const carriedValue = "carried-value-runturn"
	t.Setenv(carriedName, carriedValue)

	envCapturePath := filepath.Join(tmpDir, "env.out")
	settingCapturePath := filepath.Join(tmpDir, "setting.out")
	agentScript := writeSSHCaptureScript(t, tmpDir, carriedName, envCapturePath, settingCapturePath)

	a := &OpenCodeAdapter{}
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: agentScript},
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	a.RunTurn(context.Background(), session, domain.RunTurnParams{ //nolint:errcheck // disposition not under test here
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})

	gotEnv, err := os.ReadFile(envCapturePath)
	if err != nil {
		t.Fatalf("ReadFile(env.out): %v", err)
	}
	if string(gotEnv) != carriedValue {
		t.Errorf("carried variable = %q, want %q", string(gotEnv), carriedValue)
	}

	gotSetting, err := os.ReadFile(settingCapturePath)
	if err != nil {
		t.Fatalf("ReadFile(setting.out): %v", err)
	}
	if string(gotSetting) != "true" {
		t.Errorf("OPENCODE_DISABLE_AUTOUPDATE = %q, want %q", string(gotSetting), "true")
	}
}

func writeExportScriptWithStdinCapture(t *testing.T, dir, fixtureName string, exitCode int, stdinPath string) (string, string) {
	t.Helper()

	argsPath := filepath.Join(dir, "args.log")
	body := `cat > '` + stdinPath + `'
printf '%s\n' "$@" > '` + argsPath + `'
exit ` + strconv.Itoa(exitCode)
	if fixtureName != "" && exitCode == 0 {
		fixturePath := filepath.Join(dir, fixtureName)
		if err := os.WriteFile(fixturePath, loadFixture(t, fixtureName), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", fixtureName, err)
		}
		body = `cat > '` + stdinPath + `'
printf '%s\n' "$@" > '` + argsPath + `'
cat '` + fixturePath + `'`
	}

	return agenttest.WriteScript(t, dir, "fake-export", body), argsPath
}

func TestQueryExportUsage_LocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	// Not parallel: sets the carried variable via t.Setenv.
	const varName = "SORTIE_OPENCODE_EXPORT_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	for _, tc := range []struct {
		name        string
		sshEnvNames []string
	}{
		{"SSHEnvNames absent", nil},
		{"SSHEnvNames naming a set variable", []string{varName}},
	} {
		tmpDir := t.TempDir()
		stdinPath := filepath.Join(tmpDir, "stdin.txt")
		script, argsPath := writeExportScriptWithStdinCapture(t, tmpDir, "export_usage.json", 0, stdinPath)
		state := testExportState(script, tmpDir)
		state.target.SSHEnvNames = tc.sshEnvNames

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.Usage.InputTokens != 1750 || usage.Usage.OutputTokens != 300 {
			t.Errorf("%s: usage = %+v, want InputTokens=1750 OutputTokens=300", tc.name, usage)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(args.log): %v", tc.name, err)
		}
		const wantArgs = "session\nexport\n--standalone\n--sanitize\nses_abc123\n"
		if string(args) != wantArgs {
			t.Errorf("%s: export args = %q, want %q", tc.name, string(args), wantArgs)
		}

		stdin, err := os.ReadFile(stdinPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(stdin.txt): %v", tc.name, err)
		}
		if len(stdin) != 0 {
			t.Errorf("%s: subprocess standard input = %q, want empty on a local launch", tc.name, stdin)
		}
	}
}
