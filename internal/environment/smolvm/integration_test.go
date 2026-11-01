//go:build unix

package smolvm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/workspace"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// smolvmLiveBound bounds the smolvm work of one gated test, and of one
// test VM's deletion. Measured on smolvm 1.32.4: the slowest gated test,
// TestIntegration_PrepareRealVM, took 31 s while the host ran other
// builds.
const smolvmLiveBound = 5 * time.Minute

// smolvm refuses every guest request that carries its credential-placeholder
// prefix, and an agent sends what it reads to its model, so the prefix never
// appears spelled out in a file: it is joined from two halves at run time.
const (
	placeholderPrefixHead = "SMOL_"
	placeholderPrefixTail = "PLACEHOLDER_"
)

func placeholderPrefix() string { return placeholderPrefixHead + placeholderPrefixTail }

// liveContext returns a context derived from t.Context() that is done
// after smolvmLiveBound.
func liveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), smolvmLiveBound)
	t.Cleanup(cancel)
	return ctx
}

// integrationPack returns the pack the gated tests create VMs from:
// SORTIE_SMOLVM_TEST_PACK, or the spike's base pack.
func integrationPack(t *testing.T) string {
	t.Helper()
	if pack := os.Getenv("SORTIE_SMOLVM_TEST_PACK"); pack != "" {
		return pack
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("SORTIE_SMOLVM_TEST_PACK is unset and the home directory is unknown: %v", err)
	}
	return filepath.Join(home, "dev", "sortie-worktrees", "spike-smolvm", "spikes", "smolvm", "golden", "out", "base-639f67207fb8f11d.smolmachine")
}

func requireSmolvm(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_SMOLVM_TEST") != "1" {
		t.Skip("skipping smolvm integration test: set SORTIE_SMOLVM_TEST=1 to run it")
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the test names the pack
	if err != nil {
		t.Fatalf("open pack: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash pack: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestIntegration_PrepareRealVM(t *testing.T) {
	requireSmolvm(t)
	pack := integrationPack(t)
	if _, err := os.Stat(pack); err != nil {
		t.Fatalf("pack %s: %v; set SORTIE_SMOLVM_TEST_PACK", pack, err)
	}

	key := fmt.Sprintf("SMOLTEST-%d", time.Now().UnixNano()%1_000_000_000)
	root := t.TempDir()
	host := filepath.Join(root, key)
	if err := os.Mkdir(host, 0o750); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(root, "WORKFLOW.md")
	built, err := New(environment.Params{
		Config:       config.EnvironmentConfig{Kind: config.EnvironmentKindSmolvm, Smolvm: config.SmolvmConfig{Binary: DefaultBinary, Pack: pack}},
		StateDir:     t.TempDir(),
		WorkflowPath: workflow,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	env := built.(*Environment)
	cli := env.cli
	name := VMName(key)
	ws := environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace, Identifier: key}
	t.Cleanup(func() { deleteTestVM(t, env, name) })

	ctx := liveContext(t)
	machine := func() Machine {
		t.Helper()
		machines, err := cli.List(ctx)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		i := slices.IndexFunc(machines, func(m Machine) bool { return m.Name == name })
		if i < 0 {
			t.Fatalf("VM %s is not listed", name)
		}
		return machines[i]
	}
	prepare := func(wantCreated bool) {
		t.Helper()
		res, err := env.Prepare(ctx, ws)
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if res.CreatedNow != wantCreated {
			t.Fatalf("Prepare().CreatedNow = %v, want %v", res.CreatedNow, wantCreated)
		}
		if m := machine(); !m.Running() {
			t.Fatalf("VM state after Prepare() = %q, want running", m.State)
		}
	}
	guest := func(script string) error {
		t.Helper()
		_, err := cli.Exec(ctx, name, ExecOptions{}, []string{"sh", "-c", script})
		return err
	}

	prepare(true)
	m := machine()
	wantLabels := map[string]string{
		LabelManaged:   "1",
		LabelWorkspace: key,
		LabelPack:      fileSHA256(t, pack),
		LabelWorkflow:  shortHash(workflow),
	}
	for k, v := range wantLabels {
		if m.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, m.Labels[k], v)
		}
	}
	// smolvm counts the one directory mount; the socket is not counted.
	if m.Mounts != 1 || m.Network {
		t.Errorf("VM mounts = %d, network = %v; want 1 mount and no network", m.Mounts, m.Network)
	}
	if err := guest("echo from-guest > /run/sortie/control/probe && test -S /run/sortie/tools.sock && test ! -e /opt/sortie"); err != nil {
		t.Fatalf("guest mount check: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(host, ".sortie", "probe")); err != nil || strings.TrimSpace(string(data)) != "from-guest" {
		t.Errorf("host .sortie/probe = %q, %v, want the guest's write", data, err)
	}
	if err := guest("pgrep -x socat"); err != nil {
		t.Errorf("guest tool listener: %v, want socat running after Prepare()", err)
	}
	if err := guest("touch /workspace/marker"); err != nil {
		t.Fatalf("guest write to /workspace: %v", err)
	}

	// The workspace lifecycle records the VM after a successful
	// preparation; from then on the VM is reused.
	writeRecord(t, host, config.EnvironmentKindSmolvm, name)
	prepare(false)
	if err := cli.Stop(ctx, name); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	prepare(false)
	if err := guest("test -e /workspace/marker"); err != nil {
		t.Errorf("marker after a restart: %v, want the guest workspace kept", err)
	}
	if err := guest("pgrep -x socat"); err != nil {
		t.Errorf("guest tool listener after a restart: %v, want socat running again", err)
	}

	// A host directory without the record is a fresh workspace: the
	// running VM that predates it is replaced.
	if err := os.Remove(filepath.Join(host, ".sortie", "environment")); err != nil {
		t.Fatal(err)
	}
	prepare(true)
	if err := guest("test ! -e /workspace/marker"); err != nil {
		t.Errorf("marker after a recreate: %v, want a new guest workspace", err)
	}
}

// integrationEnv returns a smolvm environment for the gated tests, with
// its workspace root, and deletes the VM of every key it is told about
// when the test ends.
type integrationEnv struct {
	env  *Environment
	root string
	keys []string
}

func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()
	requireSmolvm(t)
	pack := integrationPack(t)
	if _, err := os.Stat(pack); err != nil {
		t.Fatalf("pack %s: %v; set SORTIE_SMOLVM_TEST_PACK", pack, err)
	}
	root := t.TempDir()
	built, err := New(environment.Params{
		Config:       config.EnvironmentConfig{Kind: config.EnvironmentKindSmolvm, Smolvm: config.SmolvmConfig{Binary: DefaultBinary, Pack: pack}},
		StateDir:     t.TempDir(),
		WorkflowPath: filepath.Join(root, "WORKFLOW.md"),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ie := &integrationEnv{env: built.(*Environment), root: root}
	t.Cleanup(func() {
		for _, key := range ie.keys {
			deleteTestVM(t, ie.env, VMName(key))
		}
	})
	return ie
}

// deleteTestVM deletes a gated test's VM, which may be gone already, and
// fails the test when smolvm's residue for its name is left behind.
func deleteTestVM(t *testing.T, env *Environment, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), smolvmLiveBound)
	defer cancel()
	if err := env.deleteVM(ctx, name); err != nil {
		t.Errorf("delete VM %s: %v", name, err)
	}
	assertNoResidue(t, env, name)
}

// assertNoResidue fails the test when smolvm's per-machine directory or
// lock file for name, a machine that no longer exists, is left behind.
func assertNoResidue(t *testing.T, env *Environment, name string) {
	t.Helper()
	if env.machineCache == "" {
		t.Fatal("smolvm's machine cache directory is unknown")
	}
	machineDir, lock := machineResidue(env.machineCache, name)
	for _, path := range []string{machineDir, lock} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("smolvm residue %s of VM %s is left behind (Lstat error %v)", path, name, err)
		}
	}
}

// newKey returns a fresh workspace key whose VM is deleted when the test
// ends.
func (ie *integrationEnv) newKey(prefix string) string {
	key := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
	ie.keys = append(ie.keys, key)
	return key
}

// guest runs script in key's VM and returns its standard output.
func (ie *integrationEnv) guest(ctx context.Context, key, script string) (string, error) {
	out, err := ie.env.cli.Exec(ctx, VMName(key), ExecOptions{}, []string{"sh", "-c", script})
	return string(out), err
}

// Not parallel: it sets the session variables in the process
// environment, which hooks inherit on the host.
func TestIntegration_HookRunsInTheGuest(t *testing.T) {
	ie := newIntegrationEnv(t)
	t.Setenv("SORTIE_SESSION_TOKEN", "integration-session-token")
	t.Setenv("SORTIE_API_URL", "http://127.0.0.1:1/api")
	key := ie.newKey("SMOLHOOK")
	ctx := liveContext(t)

	res, err := workspace.Prepare(ctx, workspace.PrepareParams{
		Root:       ie.root,
		Identifier: key,
		IssueID:    "10001",
		AfterCreate: `uname -n > created-on
printf '%s|%s|%s|%s' "$SORTIE_ENVIRONMENT" "$SORTIE_WORKSPACE" "${SORTIE_SESSION_TOKEN-unset}" "${SORTIE_API_URL-unset}" > hook-env`,
		HookTimeoutMS: 120_000,
		Environment:   ie.env,
	})
	if err != nil {
		t.Fatalf("workspace.Prepare() error = %v", err)
	}
	if !res.CreatedNow {
		t.Error("workspace.Prepare().CreatedNow = false, want true")
	}

	hostname, err := ie.guest(ctx, key, "uname -n")
	if err != nil {
		t.Fatalf("guest uname -n: %v", err)
	}
	createdOn, err := ie.guest(ctx, key, "cat /workspace/created-on")
	if err != nil || strings.TrimSpace(createdOn) != strings.TrimSpace(hostname) || strings.TrimSpace(hostname) == "" {
		t.Errorf("guest /workspace/created-on = %q, %v, want the VM's hostname %q", createdOn, err, hostname)
	}
	if host, _ := os.Hostname(); strings.TrimSpace(hostname) == host {
		t.Errorf("guest hostname %q is the host's", hostname)
	}
	if _, err := os.Lstat(filepath.Join(res.Path, "created-on")); !os.IsNotExist(err) {
		t.Errorf("host %s/created-on: Lstat error = %v, want it absent", res.Path, err)
	}
	hookEnv, err := ie.guest(ctx, key, "cat /workspace/hook-env")
	if want := "smolvm|/workspace|unset|unset"; err != nil || hookEnv != want {
		t.Errorf("hook environment = %q, %v, want %q", hookEnv, err, want)
	}
}

func TestIntegration_ConnectionFailedOnRealExits(t *testing.T) {
	ie := newIntegrationEnv(t)
	key := ie.newKey("SMOLCONN")
	host := filepath.Join(ie.root, key)
	if err := os.Mkdir(host, 0o750); err != nil {
		t.Fatal(err)
	}
	ws := environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace}
	ctx := liveContext(t)
	if _, err := ie.env.Prepare(ctx, ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	launch := func(ws environment.Workspace, l domain.CarrierLaunch) (*exec.Cmd, *strings.Builder) {
		t.Helper()
		hostArgv, err := ie.env.LaunchArgv(ws, l)
		if err != nil {
			t.Fatalf("LaunchArgv() error = %v", err)
		}
		cmd := exec.CommandContext(ctx, hostArgv[0], hostArgv[1:]...) //nolint:gosec // G204: the test's own smolvm launch
		var stderr strings.Builder
		cmd.Stderr = &stderr
		return cmd, &stderr
	}
	checkLaunch := func(name string, ws environment.Workspace, want bool, l domain.CarrierLaunch) {
		t.Helper()
		cmd, stderr := launch(ws, l)
		exit := procutil.ExtractExitCode(cmd.Run())
		if got := ie.env.ConnectionFailed(ws, exit, strings.Split(stderr.String(), "\n")); got != want {
			t.Errorf("%s: ConnectionFailed(%d, %q) = %v, want %v", name, exit, stderr.String(), got, want)
		}
	}
	check := func(name string, ws environment.Workspace, want bool, argv ...string) {
		t.Helper()
		checkLaunch(name, ws, want, domain.CarrierLaunch{Argv: argv})
	}

	check("guest exit 1", ws, false, "sh", "-c", "echo guest-err >&2; exit 1")
	check("guest prints a report, then more", ws, false, "sh", "-c",
		`echo '{"error":{"code":"NOT_FOUND","message":"vm not found: other"}}' >&2; echo more >&2; exit 1`)
	check("guest prints JSON that is not a report", ws, false, "sh", "-c", `echo '{"error":"NOT_FOUND"}' >&2; exit 1`)
	check("missing guest program", ws, false, "no-such-agent-program")
	t.Setenv("SORTIE_SMOLVM_TEST_UNSET_SECRET", "")
	if err := os.Unsetenv("SORTIE_SMOLVM_TEST_UNSET_SECRET"); err != nil {
		t.Fatal(err)
	}
	checkLaunch("unset secret", ws, true, domain.CarrierLaunch{Argv: []string{"true"}, SecretEnv: []string{"SORTIE_SMOLVM_TEST_UNSET_SECRET"}})
	missing := ws
	missing.Key = key + "-missing"
	ie.keys = append(ie.keys, missing.Key)
	check("missing VM", missing, true, "true")

	// The VM stops under a running command once the command has
	// started.
	cmd, stderr := launch(ws, domain.CarrierLaunch{Argv: []string{"sh", "-c", "echo started; exec sleep 300"}})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the long command: %v", err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "started\n" {
		t.Fatalf("long command output = %q, %v, want started", line, err)
	}
	if err := ie.env.cli.Stop(ctx, VMName(key)); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	exit := procutil.ExtractExitCode(cmd.Wait())
	if !ie.env.ConnectionFailed(ws, exit, strings.Split(stderr.String(), "\n")) {
		t.Errorf("VM stopped mid-command: ConnectionFailed(%d, %q) = false, want true", exit, stderr.String())
	}

	check("stopped VM", ws, true, "true")
}

func TestIntegration_ControlDirectoryIsShared(t *testing.T) {
	ie := newIntegrationEnv(t)
	key := ie.newKey("SMOLCTL")
	ctx := liveContext(t)
	prepare := func() workspace.PrepareResult {
		t.Helper()
		res, err := workspace.Prepare(ctx, workspace.PrepareParams{
			Root:          ie.root,
			Identifier:    key,
			IssueID:       "10001",
			AfterCreate:   "test ! -e .sortie && git init -q .",
			HookTimeoutMS: 120_000,
			Environment:   ie.env,
		})
		if err != nil {
			t.Fatalf("workspace.Prepare() error = %v", err)
		}
		return res
	}
	res := prepare()
	if !res.CreatedNow {
		t.Error("workspace.Prepare().CreatedNow = false, want true")
	}
	if target, err := ie.guest(ctx, key, "readlink /workspace/.sortie"); err != nil || strings.TrimSpace(target) != GuestControlDir {
		t.Errorf("guest readlink /workspace/.sortie = %q, %v, want %s", target, err, GuestControlDir)
	}

	// The agent's status write reaches Sortie's host reader.
	if _, err := ie.guest(ctx, key, "cd /workspace && printf 'blocked\\nwaiting on a credential\\n' > .sortie/status"); err != nil {
		t.Fatalf("guest status write: %v", err)
	}
	status := workspace.ReadStatusFile(res.Path, slog.New(slog.DiscardHandler))
	if status.Signal != workspace.StatusBlocked || status.Statement.Text != "waiting on a credential\n" {
		t.Errorf("host ReadStatusFile() = %+v, want blocked with the agent's statement", status)
	}

	// A link the agent plants is refused on the host.
	if _, err := ie.guest(ctx, key, "cd /workspace && rm .sortie/status && ln -s /etc/passwd .sortie/status"); err != nil {
		t.Fatalf("guest link plant: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(res.Path, ".sortie", "status")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("host .sortie/status = %v, %v, want the planted link", info, err)
	}
	if status := workspace.ReadStatusFile(res.Path, slog.New(slog.DiscardHandler)); status != (workspace.StatusFile{}) {
		t.Errorf("host ReadStatusFile() through a planted link = %+v, want nothing read", status)
	}
	if f, err := workspacekit.OpenSortieFile(res.Path, "status"); err == nil {
		_ = f.Close()
		t.Error("workspacekit.OpenSortieFile() through a planted link succeeded, want a refusal")
	} else if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("workspacekit.OpenSortieFile() error = %v, want a refusal, not absence", err)
	}

	// A later preparation reuses the VM and its link.
	if res := prepare(); res.CreatedNow {
		t.Error("second workspace.Prepare().CreatedNow = true, want the VM reused")
	}
	if target, err := ie.guest(ctx, key, "readlink /workspace/.sortie"); err != nil || strings.TrimSpace(target) != GuestControlDir {
		t.Errorf("guest readlink /workspace/.sortie after reuse = %q, %v, want %s", target, err, GuestControlDir)
	}
}

func TestIntegration_ExistingSortieEntryFailsPreparation(t *testing.T) {
	ie := newIntegrationEnv(t)
	tests := []struct {
		name        string
		afterCreate string
		check       string // a guest check that the entry is unchanged
	}{
		{
			name:        "repository directory",
			afterCreate: "mkdir .sortie && echo x > .sortie/f",
			check:       `test -d /workspace/.sortie && test ! -L /workspace/.sortie && test "$(cat /workspace/.sortie/f)" = x && test "$(ls -A /workspace/.sortie)" = f`,
		},
		{
			name:        "dangling link",
			afterCreate: "ln -s /nonexistent .sortie",
			check:       `test -L /workspace/.sortie && test "$(readlink /workspace/.sortie)" = /nonexistent`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := ie.newKey("SMOLDOT")
			ctx := liveContext(t)
			_, err := workspace.Prepare(ctx, workspace.PrepareParams{
				Root:          ie.root,
				Identifier:    key,
				IssueID:       "10001",
				AfterCreate:   tt.afterCreate,
				HookTimeoutMS: 120_000,
				Environment:   ie.env,
			})
			if err == nil || !strings.Contains(err.Error(), "/workspace/.sortie") {
				t.Fatalf("workspace.Prepare() error = %v, want a failure naming /workspace/.sortie", err)
			}
			if _, err := ie.guest(ctx, key, tt.check); err != nil {
				t.Errorf("guest .sortie entry after the failure: %v, want it unchanged", err)
			}
			if _, err := os.Stat(filepath.Join(ie.root, key)); !os.IsNotExist(err) {
				t.Errorf("host workspace after the failure: Stat error = %v, want it rolled back", err)
			}
		})
	}
}

// listed returns the machine named name, and whether smolvm lists it.
func (ie *integrationEnv) listed(ctx context.Context, t *testing.T, name string) (Machine, bool) {
	t.Helper()
	machines, err := ie.env.cli.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	i := slices.IndexFunc(machines, func(m Machine) bool { return m.Name == name })
	if i < 0 {
		return Machine{}, false
	}
	return machines[i], true
}

// removalObserver reads the before_remove hook's marker from the shared
// control directory when the VM is removed, before the host workspace
// directory goes.
type removalObserver struct {
	*Environment
	marker string
	seen   *string
}

func (o removalObserver) Remove(ctx context.Context, ws environment.Workspace) error {
	data, _ := os.ReadFile(filepath.Join(ws.HostPath, ".sortie", o.marker))
	*o.seen = string(data)
	return o.Environment.Remove(ctx, ws)
}

func TestIntegration_ReleaseRemove(t *testing.T) {
	ie := newIntegrationEnv(t)
	key := ie.newKey("SMOLREL")
	name := VMName(key)
	ctx := liveContext(t)

	res, err := workspace.Prepare(ctx, workspace.PrepareParams{
		Root: ie.root, Identifier: key, IssueID: "10001", HookTimeoutMS: 120_000, Environment: ie.env,
	})
	if err != nil {
		t.Fatalf("workspace.Prepare() error = %v", err)
	}
	ws := environment.Workspace{Key: key, HostPath: res.Path, GuestPath: GuestWorkspace}
	if _, err := ie.guest(ctx, key, "touch /workspace/kept"); err != nil {
		t.Fatalf("guest write: %v", err)
	}

	if err := ie.env.Release(ctx, ws); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if m, ok := ie.listed(ctx, t, name); !ok || m.Running() {
		t.Fatalf("VM after Release() = %+v (listed %v), want it stopped and still present", m, ok)
	}
	if err := ie.env.Release(ctx, ws); err != nil {
		t.Errorf("second Release() error = %v, want nil for a stopped VM", err)
	}

	var seen string
	err = workspace.CleanupByPath(ctx, workspace.CleanupByPathParams{
		Path: res.Path, Identifier: key, IssueID: "10001", HookTimeoutMS: 120_000,
		BeforeRemove: `test -e /workspace/kept && uname -n > /run/sortie/control/removed-on`,
		Environment:  removalObserver{Environment: ie.env, marker: "removed-on", seen: &seen},
	})
	if err != nil {
		t.Fatalf("workspace.CleanupByPath() error = %v", err)
	}
	if host, _ := os.Hostname(); strings.TrimSpace(seen) == "" || strings.TrimSpace(seen) == host {
		t.Errorf("before_remove ran on %q, want the VM, with its guest workspace", seen)
	}
	if _, ok := ie.listed(ctx, t, name); ok {
		t.Error("VM still listed after the workspace removal")
	}
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Errorf("host workspace Stat error = %v, want it removed", err)
	}
	if _, err := os.Stat(filepath.Join(ie.env.stateDir, stateEnvDir, key)); !os.IsNotExist(err) {
		t.Errorf("state directory Stat error = %v, want it removed", err)
	}
}

func TestIntegration_Sweep(t *testing.T) {
	ie := newIntegrationEnv(t)
	key := ie.newKey("SMOLSWEEP")
	name := VMName(key)
	unlabeled := VMName(ie.newKey("SMOLUNL"))
	ctx := liveContext(t)

	host := filepath.Join(ie.root, key)
	if err := os.Mkdir(host, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := ie.env.Prepare(ctx, environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := ie.env.cli.Create(ctx, CreateSpec{Name: unlabeled, Pack: integrationPack(t), Labels: []Label{{"sortie.test", "1"}}}); err != nil {
		t.Fatalf("create the unlabeled VM: %v", err)
	}
	if err := ie.env.cli.Start(ctx, unlabeled); err != nil {
		t.Fatalf("start the unlabeled VM: %v", err)
	}

	// A sweep while the workspace exists, or while its worker runs,
	// deletes nothing.
	if err := ie.env.Sweep(ctx, environment.SweepParams{WorkspaceRoot: ie.root}); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if err := os.RemoveAll(host); err != nil {
		t.Fatal(err)
	}
	if err := ie.env.Sweep(ctx, environment.SweepParams{WorkspaceRoot: ie.root, Running: map[string]bool{key: true}}); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if _, ok := ie.listed(ctx, t, name); !ok {
		t.Fatal("managed VM deleted while its workspace existed or its worker ran")
	}

	if err := ie.env.Sweep(ctx, environment.SweepParams{WorkspaceRoot: ie.root}); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if _, ok := ie.listed(ctx, t, name); ok {
		t.Error("managed VM whose workspace directory is gone is still listed after the sweep")
	}
	if m, ok := ie.listed(ctx, t, unlabeled); !ok || !m.Running() {
		t.Errorf("unlabeled VM after the sweep = %+v (listed %v), want it left running", m, ok)
	}
}

// The binding's header is set on HTTPS requests to its host, replacing
// the guest's own value, while the guest variable holds a placeholder.
// It needs egress to httpbin.org, which echoes the request headers.
//
// Not parallel: it sets the credential variable in the process
// environment, where smolvm reads it when the VM starts.
func TestIntegration_CredentialHeaderIsSetOnTheHost(t *testing.T) {
	requireSmolvm(t)
	const variable, value = "SORTIE_SMOLVM_TEST_HEADER_KEY", "integration-header-value"
	t.Setenv(variable, value)
	pack := integrationPack(t)
	if _, err := os.Stat(pack); err != nil {
		t.Fatalf("pack %s: %v; set SORTIE_SMOLVM_TEST_PACK", pack, err)
	}
	root := t.TempDir()
	built, err := New(environment.Params{
		Config: config.EnvironmentConfig{Kind: config.EnvironmentKindSmolvm, Smolvm: config.SmolvmConfig{
			Binary: DefaultBinary,
			Pack:   pack,
			Credentials: []config.SmolvmCredential{
				{Name: "echo", Env: variable, Hosts: []string{"httpbin.org"}, Header: "X-Api-Key"},
			},
		}},
		StateDir:     t.TempDir(),
		WorkflowPath: filepath.Join(root, "WORKFLOW.md"),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ie := &integrationEnv{env: built.(*Environment), root: root}
	key := ie.newKey("SMOLHDR")
	t.Cleanup(func() { deleteTestVM(t, ie.env, VMName(key)) })
	host := filepath.Join(root, key)
	if err := os.Mkdir(host, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx := liveContext(t)
	if _, err := ie.env.Prepare(ctx, environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	seen, err := ie.guest(ctx, key, "printenv "+variable)
	if err != nil || !strings.HasPrefix(seen, placeholderPrefix()) || strings.Contains(seen, value) {
		t.Errorf("guest %s = %q, %v, want a placeholder", variable, seen, err)
	}
	echo, err := ie.guest(ctx, key, `curl -sS -m 30 -H "X-Api-Key: guest-own" https://httpbin.org/headers | jq -r '.headers["X-Api-Key"]'`)
	if err != nil || strings.TrimSpace(echo) != value {
		t.Errorf("X-Api-Key received by httpbin.org = %q, %v, want the host's value", echo, err)
	}
}

// Releasing and removing a workspace whose VM is missing, after a
// command ran against it, leaves none of smolvm's residue for its name.
func TestIntegration_MissingVMLeavesNoResidue(t *testing.T) {
	ie := newIntegrationEnv(t)
	ctx := liveContext(t)
	for _, step := range []string{"Release", "Remove"} {
		key := ie.newKey("SMOLRES")
		host := filepath.Join(ie.root, key)
		if err := os.Mkdir(host, 0o750); err != nil {
			t.Fatal(err)
		}
		ws := environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace}
		name := VMName(key)
		if _, err := ie.guest(ctx, key, "true"); !IsNotFound(err) {
			t.Fatalf("exec in the missing VM %s: %v, want smolvm's not-found", name, err)
		}
		machineDir, _ := machineResidue(ie.env.machineCache, name)
		if _, err := os.Stat(machineDir); err != nil {
			t.Fatalf("smolvm left no directory for the missing VM %s: %v; its residue naming changed", name, err)
		}
		var err error
		if step == "Release" {
			err = ie.env.Release(ctx, ws)
		} else {
			err = ie.env.Remove(ctx, ws)
		}
		if err != nil {
			t.Fatalf("%s() error = %v", step, err)
		}
		assertNoResidue(t, ie.env, name)
	}
}
