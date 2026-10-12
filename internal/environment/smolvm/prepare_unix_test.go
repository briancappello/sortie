//go:build unix

package smolvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

const testPackContent = "a smolmachine pack"

// prepareFixture is an environment driven by a fake smolvm, with one
// host workspace.
type prepareFixture struct {
	fake     *fakeSmolvm
	env      *Environment
	ws       environment.Workspace
	stateDir string
	pack     string
	workflow string
}

func newPrepareFixture(t *testing.T, state fakeState, mutate func(*config.SmolvmConfig)) prepareFixture {
	t.Helper()
	fake := newFakeSmolvm(t, state)
	dir := t.TempDir()
	pack := filepath.Join(dir, "base.smolmachine")
	if err := os.WriteFile(pack, []byte(testPackContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.SmolvmConfig{Binary: fake.Binary, Pack: pack}
	if mutate != nil {
		mutate(&cfg)
	}
	root := filepath.Join(dir, "workspaces")
	host := filepath.Join(root, "VM-1")
	if err := os.MkdirAll(host, 0o750); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	env, err := New(environment.Params{
		Config:       config.EnvironmentConfig{Kind: config.EnvironmentKindSmolvm, Smolvm: cfg},
		StateDir:     stateDir,
		WorkflowPath: workflow,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return prepareFixture{
		fake:     fake,
		env:      env.(*Environment),
		ws:       environment.Workspace{Key: "VM-1", HostPath: host, GuestPath: GuestWorkspace, Identifier: "VM-1"},
		stateDir: stateDir,
		pack:     pack,
		workflow: workflow,
	}
}

// writeRecord writes the workspace's environment record as the
// workspace lifecycle does after a successful preparation.
func writeRecord(t *testing.T, hostPath, kind, name string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"kind": kind, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(hostPath, ".sortie")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "environment"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func managedMachine(name, key, state string) fakeMachine {
	return fakeMachine{
		Name:   name,
		State:  state,
		Labels: map[string]string{LabelManaged: "1", LabelWorkspace: key},
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var listArgv = []string{"machine", "ls", "--json"}

func TestPrepare_CreatesAndStartsTheVM(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.CPUs = 2
		cfg.MemoryMiB = 4096
		cfg.StorageGiB = 20
		cfg.AllowHosts = []string{"models.opencode.ai", "*.npmjs.org", "openrouter.ai"}
		cfg.Credentials = []config.SmolvmCredential{
			{Name: "openrouter", Env: "OPENROUTER_API_KEY", Hosts: []string{"openrouter.ai"}},
			{Name: "github", Env: "GH_TOKEN", Hosts: []string{"api.github.com", "*.githubusercontent.com"}},
		}
		cfg.SSHAgent = true
	})

	res, err := f.env.Prepare(context.Background(), f.ws)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !res.CreatedNow {
		t.Error("Prepare().CreatedNow = false, want true after a create")
	}

	control := filepath.Join(f.ws.HostPath, ".sortie")
	binDir := filepath.Join(f.stateDir, "env", "bin")
	socket := filepath.Join(f.stateDir, "env", "VM-1", "tools.sock")
	wantCreate := []string{
		"machine", "create", "--name", "sortie-VM-1", "--from", f.pack,
		"--cpus", "2", "--mem", "4096", "--storage", "20",
		"--label", "sortie.managed=1",
		"--label", "sortie.workspace=VM-1",
		"--label", "sortie.pack=" + sha256Hex(testPackContent),
		"--label", "sortie.workflow=" + shortHash(f.workflow),
		"--allow-host-pattern", "models.opencode.ai",
		"--allow-host-pattern", "*.npmjs.org",
		"--allow-host-pattern", "openrouter.ai",
		"--allow-host-pattern", "api.github.com",
		"--allow-host-pattern", "*.githubusercontent.com",
		"--credential", "openrouter=OPENROUTER_API_KEY@openrouter.ai",
		"--credential", "github=GH_TOKEN@api.github.com,*.githubusercontent.com",
		"--ssh-agent",
		"-v", control + ":/run/sortie/control",
		"-v", binDir + ":/opt/sortie:ro",
		"--mount-socket", socket + ":/run/sortie/tools.sock",
	}
	want := [][]string{listArgv, wantCreate, {"machine", "start", "--name", "sortie-VM-1"}}
	got := f.fake.argvs()
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
	assertOnlyGrantedResources(t, got[1])

	for _, dir := range []string{control, binDir, filepath.Dir(socket)} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("Stat(%s) = %v, %v, want a directory created before the VM", dir, info, err)
		}
	}
	if info, err := os.Stat(filepath.Dir(socket)); err == nil && info.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode = %v, want 0700", info.Mode().Perm())
	}
}

// assertOnlyGrantedResources fails unless create holds exactly the two
// directory mounts and the socket mount, and none of the flags that give
// a VM more of the host.
func assertOnlyGrantedResources(t *testing.T, create []string) {
	t.Helper()
	forbidden := []string{
		"--nested", "--allow-system-mounts", "--docker-socket", "-p", "--port", "--volume",
		"--disk", "--cache-disk", "--expose-socket", "--gpu", "--cuda", "--use-host-proxy",
		"--trust-host-certs", "--net", "--allow-host", "--allow-cidr", "--outbound-localhost-only",
		"-e", "--env", "--secret-env", "--secret-file", "--init", "--smolfile", "-s",
	}
	counts := map[string]int{}
	for _, arg := range create {
		if slices.Contains(forbidden, arg) {
			t.Errorf("create argv contains %q", arg)
		}
		counts[arg]++
	}
	if counts["-v"] != 2 || counts["--mount-socket"] != 1 {
		t.Errorf("create argv has %d -v and %d --mount-socket, want 2 and 1", counts["-v"], counts["--mount-socket"])
	}
}

func TestPrepare_NoNetworkWithoutAllowHostsOrCredentials(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	if _, err := f.env.Prepare(context.Background(), f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	create := f.fake.argvs()[1]
	assertOnlyGrantedResources(t, create)
	for _, arg := range create {
		if arg == "--allow-host-pattern" || arg == "--credential" || arg == "--ssh-agent" || arg == "--cpus" || arg == "--mem" || arg == "--storage" {
			t.Errorf("create argv contains %q without a setting that asks for it", arg)
		}
	}
	if m := f.fake.state().Machines; len(m) != 1 || m[0].Network {
		t.Errorf("machines = %+v, want one VM without network", m)
	}
}

func TestPrepare_CredentialHostsJoinTheAllowlist(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.Credentials = []config.SmolvmCredential{{Name: "or", Env: "OPENROUTER_API_KEY", Hosts: []string{"openrouter.ai"}}}
	})
	if _, err := f.env.Prepare(context.Background(), f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	create := strings.Join(f.fake.argvs()[1], " ")
	if !strings.Contains(create, "--allow-host-pattern openrouter.ai --credential or=OPENROUTER_API_KEY@openrouter.ai") {
		t.Errorf("create argv = %q, want the credential host on the allowlist", create)
	}
}

// Not parallel: it sets a process environment variable the fake reads.
func TestPrepare_StartSeesSortiesEnvironment(t *testing.T) {
	const variable = "SORTIE_SMOLVM_TEST_CREDENTIAL"
	t.Setenv(variable, "placeholder-value")
	f := newPrepareFixture(t, fakeState{RecordEnv: []string{variable}}, func(cfg *config.SmolvmConfig) {
		cfg.Credentials = []config.SmolvmCredential{{Name: "c", Env: variable, Hosts: []string{"example.org"}}}
	})
	if _, err := f.env.Prepare(context.Background(), f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	calls := f.fake.calls()
	start := calls[len(calls)-1]
	if start.Args[1] != "start" || start.Env[variable] != "placeholder-value" {
		t.Errorf("last call = %+v, want machine start with %s in its environment", start, variable)
	}
}

func TestPrepare_RunningVMIsLeftAlone(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{managedMachine("sortie-VM-1", "VM-1", StateRunning)}}, nil)
	writeRecord(t, f.ws.HostPath, "smolvm", "sortie-VM-1")

	res, err := f.env.Prepare(context.Background(), f.ws)
	if err != nil || res.CreatedNow {
		t.Fatalf("Prepare() = %+v, %v, want CreatedNow false", res, err)
	}
	if got := f.fake.argvs(); !slices.EqualFunc(got, [][]string{listArgv}, slices.Equal[[]string]) {
		t.Errorf("calls = %q, want only the listing", got)
	}
}

func TestPrepare_StoppedVMIsStarted(t *testing.T) {
	t.Parallel()

	for _, state := range []string{StateStopped, StateCreated} {
		f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{managedMachine("sortie-VM-1", "VM-1", state)}}, nil)
		writeRecord(t, f.ws.HostPath, "smolvm", "sortie-VM-1")

		res, err := f.env.Prepare(context.Background(), f.ws)
		if err != nil || res.CreatedNow {
			t.Fatalf("Prepare() with a %s VM = %+v, %v, want CreatedNow false", state, res, err)
		}
		want := [][]string{listArgv, {"machine", "start", "--name", "sortie-VM-1"}}
		if got := f.fake.argvs(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
			t.Errorf("calls with a %s VM = %q, want %q", state, got, want)
		}
	}
}

func TestPrepare_VMPredatingTheWorkspaceIsRecreated(t *testing.T) {
	t.Parallel()

	for name, record := range map[string]func(t *testing.T, host string){
		"no record":    func(*testing.T, string) {},
		"another VM":   func(t *testing.T, host string) { writeRecord(t, host, "smolvm", "sortie-VM-2") },
		"another kind": func(t *testing.T, host string) { writeRecord(t, host, "local", "") },
		"unreadable record": func(t *testing.T, host string) {
			writeRecord(t, host, "smolvm", "sortie-VM-1")
			mustSymlinkRecord(t, host)
		},
		"no record, running": func(*testing.T, string) {},
	} {
		state := StateStopped
		if strings.HasSuffix(name, "running") {
			state = StateRunning
		}
		f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{managedMachine("sortie-VM-1", "VM-1", state)}}, nil)
		record(t, f.ws.HostPath)

		res, err := f.env.Prepare(context.Background(), f.ws)
		if err != nil || !res.CreatedNow {
			t.Fatalf("%s: Prepare() = %+v, %v, want CreatedNow true", name, res, err)
		}
		got := f.fake.argvs()
		if len(got) != 4 || !slices.Equal(got[1], []string{"machine", "delete", "-f", "--name", "sortie-VM-1"}) ||
			got[2][1] != "create" || got[3][1] != "start" {
			t.Errorf("%s: calls = %q, want listing, delete, create, start", name, got)
		}
	}
}

// mustSymlinkRecord replaces the record with a link, which the record
// reader refuses.
func mustSymlinkRecord(t *testing.T, host string) {
	t.Helper()
	record := filepath.Join(host, ".sortie", "environment")
	target := filepath.Join(host, ".sortie", "elsewhere")
	if err := os.Rename(record, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, record); err != nil {
		t.Fatal(err)
	}
}

func TestPrepare_ForeignVMIsNotTouched(t *testing.T) {
	t.Parallel()

	for name, labels := range map[string]map[string]string{
		"unlabeled":       {"owner": "someone"},
		"other workspace": {LabelManaged: "1", LabelWorkspace: "VM-2"},
	} {
		f := newPrepareFixture(t, fakeState{Machines: []fakeMachine{{Name: "sortie-VM-1", State: StateRunning, Labels: labels}}}, nil)

		res, err := f.env.Prepare(context.Background(), f.ws)
		if err == nil || res.CreatedNow || !strings.Contains(err.Error(), "sortie-VM-1") {
			t.Errorf("%s: Prepare() = %+v, %v, want an error naming the VM", name, res, err)
		}
		if got := f.fake.argvs(); !slices.EqualFunc(got, [][]string{listArgv}, slices.Equal[[]string]) {
			t.Errorf("%s: calls = %q, want only the listing", name, got)
		}
	}
}

func TestPrepare_SmolvmFailureFailsPreparation(t *testing.T) {
	t.Parallel()

	for _, sub := range []string{"machine ls", "machine create", "machine start"} {
		f := newPrepareFixture(t, fakeState{Fail: map[string]fakeFailure{sub: {Exit: 1, Stderr: "Error: no space left\n"}}}, nil)

		res, err := f.env.Prepare(context.Background(), f.ws)
		var cliErr *CLIError
		if !errors.As(err, &cliErr) || strings.Join(cliErr.Args, " ") != sub || res.CreatedNow {
			t.Errorf("%s failing: Prepare() = %+v, %v, want CreatedNow false and that invocation's *CLIError", sub, res, err)
		}
		if got := f.fake.argvs(); got[len(got)-1][1] != strings.TrimPrefix(sub, "machine ") {
			t.Errorf("%s failing: calls = %q, want nothing after the failure", sub, got)
		}
	}
}

func TestPrepare_RequiresAPack(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) { cfg.Pack = "" })
	_, err := f.env.Prepare(context.Background(), f.ws)
	var cfgErr *config.ConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Field != "environment.smolvm.pack" {
		t.Errorf("Prepare() error = %v, want a config error on environment.smolvm.pack", err)
	}
	if got := f.fake.argvs(); len(got) != 0 {
		t.Errorf("calls = %q, want none", got)
	}
}

func TestPrepare_RefusesKeysThatCannotNameAStateDirectory(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"", "bin", "..", "a/b"} {
		f := newPrepareFixture(t, fakeState{}, nil)
		ws := f.ws
		ws.Key = key
		if _, err := f.env.Prepare(context.Background(), ws); err == nil {
			t.Errorf("Prepare(key %q) error = nil, want a refusal", key)
		}
		if got := f.fake.argvs(); len(got) != 0 {
			t.Errorf("Prepare(key %q) calls = %q, want none", key, got)
		}
	}
}

func TestPrepare_RefusesASocketPathTooLongToBind(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	ws := f.ws
	ws.Key = strings.Repeat("K", 100)
	if _, err := f.env.Prepare(context.Background(), ws); err == nil || !strings.Contains(err.Error(), "tools.sock") {
		t.Errorf("Prepare() error = %v, want a refusal naming the socket path", err)
	}
}

func TestPackSHA256FollowsTheFile(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	first, err := f.env.packSHA256(f.pack)
	if err != nil || first != sha256Hex(testPackContent) {
		t.Fatalf("packSHA256() = %q, %v, want %q", first, err, sha256Hex(testPackContent))
	}
	if err := os.WriteFile(f.pack, []byte("a rebuilt pack, longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if second, err := f.env.packSHA256(f.pack); err != nil || second != sha256Hex("a rebuilt pack, longer") {
		t.Errorf("packSHA256() after a rebuild = %q, %v, want the new file's hash", second, err)
	}
}
