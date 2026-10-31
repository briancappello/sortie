//go:build unix

package smolvm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

func TestCLIVersion(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{Version: "smolvm 1.26.1"})
	got, err := NewCLI(fake.Binary).Version(context.Background())
	if err != nil || got != "1.26.1" {
		t.Errorf("Version() = %q, %v, want 1.26.1", got, err)
	}

	fake = newFakeSmolvm(t, fakeState{Version: "something else 1.26.1"})
	if got, err := NewCLI(fake.Binary).Version(context.Background()); err == nil {
		t.Errorf("Version() = %q, want an error for output that names no smolvm version", got)
	}
}

func TestCheckHost_RefusesUnsupportedVersion(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{Version: "smolvm 1.26.1"})
	err := CheckHost(smolvmConfig(fake.Binary))
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Field != "environment.smolvm.binary" {
		t.Fatalf("CheckHost() = %v, want a config error on environment.smolvm.binary", err)
	}
	if !strings.Contains(err.Error(), "1.26.1") || !strings.Contains(err.Error(), "1.32.4") {
		t.Errorf("CheckHost() = %q, want it to name 1.26.1 and 1.32.4", err)
	}
	if calls := fake.argvs(); len(calls) != 1 || !slices.Equal(calls[0], []string{"--version"}) {
		t.Errorf("calls = %q, want exactly one --version", calls)
	}
}

func TestCheckHost_RefusesMissingBinary(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "smolvm")
	err := CheckHost(smolvmConfig(missing))
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Field != "environment.smolvm.binary" {
		t.Fatalf("CheckHost() = %v, want a config error on environment.smolvm.binary", err)
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "1.32.4") {
		t.Errorf("CheckHost() = %q, want it to name %s and 1.32.4", err, missing)
	}
}

func TestCheckHost_RefusesFailingVersionCommand(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{Fail: map[string]fakeFailure{"--version": {Exit: 3, Stderr: "boom\n"}}})
	err := CheckHost(smolvmConfig(fake.Binary))
	if err == nil || !strings.Contains(err.Error(), "exit code 3") || !strings.Contains(err.Error(), "1.32.4") {
		t.Errorf("CheckHost() = %v, want a refusal naming exit code 3 and 1.32.4", err)
	}
}

func TestCheckHost_AcceptsSupportedVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"smolvm 1.32.4", "smolvm 2.0.0"} {
		fake := newFakeSmolvm(t, fakeState{Version: version})
		if err := CheckHost(smolvmConfig(fake.Binary)); err != nil {
			t.Errorf("CheckHost() with %q = %v, want nil", version, err)
		}
	}
}

func TestCheckHost_RefusesUnparsableVersion(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{Version: "smolvm 1.32"})
	err := CheckHost(smolvmConfig(fake.Binary))
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Field != "environment.smolvm.binary" {
		t.Fatalf("CheckHost() = %v, want a config error on environment.smolvm.binary", err)
	}
	if !strings.Contains(err.Error(), "1.32.4") {
		t.Errorf("CheckHost() = %q, want it to name the minimum 1.32.4", err)
	}
}

func TestCheckHost_AcceptsCredentialHeader(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{})
	cfg := smolvmConfig(fake.Binary)
	cfg.Smolvm.Credentials = []config.SmolvmCredential{
		{Name: "b", Env: "B_KEY", Hosts: []string{"b.example"}, Header: "X-Api-Key"},
	}
	if err := CheckHost(cfg); err != nil {
		t.Errorf("CheckHost() = %v, want a credential header accepted", err)
	}
}

func TestCheckHost_RunsThroughTheRegistry(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{Version: "smolvm 1.26.1"})
	err := environment.CheckHost(smolvmConfig(fake.Binary))
	if err == nil || !strings.Contains(err.Error(), "1.26.1") {
		t.Errorf("environment.CheckHost() = %v, want the smolvm kind's version refusal", err)
	}
}

func TestCLIList_ParsesRecordedListing(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "ls-running.json"))
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeSmolvm(t, fakeState{ListOutput: string(data)})
	machines, err := NewCLI(fake.Binary).List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(machines) != 2 || machines[0].Name != "ag1" || !machines[1].Running() {
		t.Errorf("List() = %+v, want ag1 and a running ee-fixture", machines)
	}
	if calls := fake.argvs(); len(calls) != 1 || !slices.Equal(calls[0], []string{"machine", "ls", "--error-format", "json", "--json"}) {
		t.Errorf("calls = %q, want machine ls --json", calls)
	}
}

func TestCLI_LifecycleArgv(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{})
	cli := NewCLI(fake.Binary)
	ctx := context.Background()
	spec := CreateSpec{Name: "sortie-VM-1", Pack: "/packs/base.smolmachine"}
	if err := cli.Create(ctx, spec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := cli.Start(ctx, "sortie-VM-1"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	out, err := cli.Exec(ctx, "sortie-VM-1", ExecOptions{Workdir: "/workspace"}, []string{"uname", "-n"})
	if err != nil || strings.TrimSpace(string(out)) != "uname -n" {
		t.Fatalf("Exec() = %q, %v, want the fake's echo of the command", out, err)
	}
	if err := cli.Stop(ctx, "sortie-VM-1"); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// smolvm exits 0 when asked to stop a machine that is not running.
	if err := cli.Stop(ctx, "sortie-VM-1"); err != nil {
		t.Fatalf("second Stop() error = %v, want nil", err)
	}
	if err := cli.Delete(ctx, "sortie-VM-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	want := [][]string{
		{"machine", "create", "--error-format", "json", "--name", "sortie-VM-1", "--from", "/packs/base.smolmachine"},
		{"machine", "start", "--error-format", "json", "--name", "sortie-VM-1"},
		{"machine", "exec", "--error-format", "json", "--name", "sortie-VM-1", "-w", "/workspace", "--", "uname", "-n"},
		{"machine", "stop", "--error-format", "json", "--name", "sortie-VM-1"},
		{"machine", "stop", "--error-format", "json", "--name", "sortie-VM-1"},
		{"machine", "delete", "--error-format", "json", "-f", "--name", "sortie-VM-1"},
	}
	if got := fake.argvs(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestCLI_WrapsFailures(t *testing.T) {
	t.Parallel()

	fake := newFakeSmolvm(t, fakeState{})
	cli := NewCLI(fake.Binary)
	ctx := context.Background()

	for name, op := range map[string]func() error{
		"machine start":  func() error { return cli.Start(ctx, "gone") },
		"machine delete": func() error { return cli.Delete(ctx, "gone") },
		"machine exec": func() error {
			_, err := cli.Exec(ctx, "gone", ExecOptions{}, []string{"true"})
			return err
		},
	} {
		err := op()
		var cliErr *CLIError
		if !errors.As(err, &cliErr) {
			t.Fatalf("%s: error = %v, want a *CLIError", name, err)
		}
		if strings.Join(cliErr.Args, " ") != name || cliErr.Machine != "gone" || cliErr.ExitCode != 1 {
			t.Errorf("%s: CLIError = %+v, want Args %q, machine gone, exit code 1", name, cliErr, name)
		}
		if !IsNotFound(err) || cliErr.Code != "NOT_FOUND" || !strings.Contains(err.Error(), "NOT_FOUND: vm not found: gone") {
			t.Errorf("%s: error = %q, want a not-found error quoting smolvm", name, err)
		}
	}
}

func TestCLI_KeepsLastEightKiBOfStderr(t *testing.T) {
	t.Parallel()

	stderr := strings.Repeat("x", 20*1024) + "\nError: the real cause\n"
	fake := newFakeSmolvm(t, fakeState{Fail: map[string]fakeFailure{"machine create": {Exit: 1, Stderr: stderr}}})
	err := NewCLI(fake.Binary).Create(context.Background(), CreateSpec{Name: "n", Pack: "/p"})
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("Create() error = %v, want a *CLIError", err)
	}
	if len(cliErr.Stderr) != 8*1024 || !strings.HasSuffix(cliErr.Stderr, "Error: the real cause\n") {
		t.Errorf("Stderr holds %d bytes ending %q, want the last 8192 bytes", len(cliErr.Stderr), cliErr.Stderr[max(0, len(cliErr.Stderr)-30):])
	}
	if !strings.HasSuffix(err.Error(), "Error: the real cause") {
		t.Errorf("Error() = %q, want it to end with the error line", err)
	}
}

func TestCLI_MissingBinary(t *testing.T) {
	t.Parallel()

	err := NewCLI(filepath.Join(t.TempDir(), "smolvm")).Start(context.Background(), "x")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.ExitCode != -1 {
		t.Fatalf("Start() error = %v, want a *CLIError with exit code -1", err)
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Start() error = %v, want it to wrap the missing-file error", err)
	}
}

func smolvmConfig(binary string) config.EnvironmentConfig {
	return config.EnvironmentConfig{
		Kind:   config.EnvironmentKindSmolvm,
		Smolvm: config.SmolvmConfig{Binary: binary, Pack: "/packs/base.smolmachine"},
	}
}
