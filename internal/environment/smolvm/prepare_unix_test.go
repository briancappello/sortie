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

// managedVM1 is a VM labeled as managed for the fixture's workspace
// key, VM-1, in state, without a workflow label: a machine for tests
// that never look at its owner. [prepareFixture.ownedVM] is the
// fixture's own VM.
func managedVM1(state string) fakeMachine {
	return fakeMachine{
		Name:   "sortie-VM-1",
		State:  state,
		Labels: map[string]string{LabelManaged: "1", LabelWorkspace: "VM-1"},
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var listArgv = []string{"machine", "ls", "--json"}

// toolListenerCalls are the calls that start the guest side of the tool
// channel in the running VM sortie-VM-1: the socat check, then the
// detached listener.
func toolListenerCalls() [][]string {
	return [][]string{
		append([]string{"machine", "exec", "--name", "sortie-VM-1", "--"}, toolListenerCheck...),
		append([]string{"machine", "exec", "--name", "sortie-VM-1", "-d", "--"}, toolListenerArgv...),
	}
}

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
		"--mount-socket", socket + ":/run/sortie/tools.sock",
	}
	want := append([][]string{listArgv, wantCreate, {"machine", "start", "--name", "sortie-VM-1"}}, toolListenerCalls()...)
	got := f.fake.argvs()
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
	assertOnlyGrantedResources(t, got[1])

	if _, err := os.Stat(filepath.Join(f.stateDir, "env-bin")); err == nil {
		t.Error("Prepare() created env-bin, want no binary directory: the guest needs no Sortie binary")
	}
	for _, dir := range []string{control, filepath.Dir(socket)} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("Stat(%s) = %v, %v, want a directory created before the VM", dir, info, err)
		}
	}
	if info, err := os.Stat(filepath.Dir(socket)); err == nil && info.Mode().Perm() != 0o700 {
		t.Errorf("socket directory mode = %v, want 0700", info.Mode().Perm())
	}
}

// assertOnlyGrantedResources fails unless create holds exactly the control
// directory mount and the socket mount, and none of the flags that give a
// VM more of the host.
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
	if counts["-v"] != 1 || counts["--mount-socket"] != 1 {
		t.Errorf("create argv has %d -v and %d --mount-socket, want 1 and 1", counts["-v"], counts["--mount-socket"])
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
	start := calls[len(calls)-1-len(toolListenerCalls())]
	if start.Args[1] != "start" || start.Env[variable] != "placeholder-value" {
		t.Errorf("last call = %+v, want machine start with %s in its environment", start, variable)
	}
}

func TestPrepare_RunningVMIsLeftAlone(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("VM-1", StateRunning)}})
	writeRecord(t, f.ws.HostPath, "smolvm", "sortie-VM-1")

	res, err := f.env.Prepare(context.Background(), f.ws)
	if err != nil || res.CreatedNow {
		t.Fatalf("Prepare() = %+v, %v, want CreatedNow false", res, err)
	}
	want := append([][]string{listArgv}, toolListenerCalls()...)
	if got := f.fake.argvs(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("calls = %q, want the listing and the tool listener %q", got, want)
	}
}

func TestPrepare_StoppedVMIsStarted(t *testing.T) {
	t.Parallel()

	for _, state := range []string{StateStopped, StateCreated} {
		f := newPrepareFixture(t, fakeState{}, nil)
		f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("VM-1", state)}})
		writeRecord(t, f.ws.HostPath, "smolvm", "sortie-VM-1")

		res, err := f.env.Prepare(context.Background(), f.ws)
		if err != nil || res.CreatedNow {
			t.Fatalf("Prepare() with a %s VM = %+v, %v, want CreatedNow false", state, res, err)
		}
		want := append([][]string{listArgv, {"machine", "start", "--name", "sortie-VM-1"}}, toolListenerCalls()...)
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
		f := newPrepareFixture(t, fakeState{}, nil)
		f.fake.setState(fakeState{Machines: []fakeMachine{f.ownedVM("VM-1", state)}})
		record(t, f.ws.HostPath)

		res, err := f.env.Prepare(context.Background(), f.ws)
		if err != nil || !res.CreatedNow {
			t.Fatalf("%s: Prepare() = %+v, %v, want CreatedNow true", name, res, err)
		}
		got := f.fake.argvs()
		if len(got) != 6 || !slices.Equal(got[1], []string{"machine", "delete", "-f", "--name", "sortie-VM-1"}) ||
			got[2][1] != "create" || got[3][1] != "start" ||
			!slices.EqualFunc(got[4:], toolListenerCalls(), slices.Equal[[]string]) {
			t.Errorf("%s: calls = %q, want listing, delete, create, start, and the tool listener", name, got)
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

	for name, labels := range map[string]func(f prepareFixture) map[string]string{
		"unlabeled": func(prepareFixture) map[string]string { return map[string]string{"owner": "someone"} },
		"other workspace": func(f prepareFixture) map[string]string {
			return map[string]string{LabelManaged: "1", LabelWorkspace: "VM-2", LabelWorkflow: shortHash(f.workflow)}
		},
		"another workflow": func(prepareFixture) map[string]string {
			return map[string]string{LabelManaged: "1", LabelWorkspace: "VM-1", LabelWorkflow: "0123456789ab"}
		},
		"no workflow label": func(prepareFixture) map[string]string {
			return map[string]string{LabelManaged: "1", LabelWorkspace: "VM-1"}
		},
		"not managed": func(f prepareFixture) map[string]string {
			return map[string]string{LabelWorkspace: "VM-1", LabelWorkflow: shortHash(f.workflow)}
		},
	} {
		// The record names the VM or nothing: neither lets Prepare
		// start, reuse, or replace a machine it does not own.
		for _, withRecord := range []bool{false, true} {
			f := newPrepareFixture(t, fakeState{}, nil)
			f.fake.setState(fakeState{Machines: []fakeMachine{{Name: "sortie-VM-1", State: StateStopped, Labels: labels(f)}}})
			if withRecord {
				writeRecord(t, f.ws.HostPath, "smolvm", "sortie-VM-1")
			}

			res, err := f.env.Prepare(context.Background(), f.ws)
			if err == nil || res.CreatedNow || !strings.Contains(err.Error(), "sortie-VM-1") ||
				!strings.Contains(err.Error(), "another workflow") {
				t.Errorf("%s (record %v): Prepare() = %+v, %v, want an error naming the VM and its owner", name, withRecord, res, err)
			}
			if got := f.fake.argvs(); !slices.EqualFunc(got, [][]string{listArgv}, slices.Equal[[]string]) {
				t.Errorf("%s (record %v): calls = %q, want only the listing", name, withRecord, got)
			}
			if m := f.fake.state().Machines; len(m) != 1 || m[0].State != StateStopped {
				t.Errorf("%s (record %v): machines = %+v, want the machine untouched", name, withRecord, m)
			}
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

	for _, key := range []string{"", ".", "..", "a/b"} {
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

func TestPrepare_KeyBinKeepsItsOwnStateDirectory(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	ws := f.ws
	ws.Key = "bin"
	if _, err := f.env.Prepare(context.Background(), ws); err != nil {
		t.Fatalf("Prepare(key bin) error = %v", err)
	}
	create := strings.Join(f.fake.argvs()[1], " ")
	socket := filepath.Join(f.stateDir, "env", "bin", "tools.sock")
	if !strings.Contains(create, "--mount-socket "+socket+":") {
		t.Errorf("create argv = %q, want the socket %s", create, socket)
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

func TestPrepare_ReportsThePackDigestTheVMRecords(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	res, err := f.env.Prepare(context.Background(), f.ws)
	if err != nil || res.ImageDigest != sha256Hex(testPackContent) {
		t.Fatalf("Prepare() of a new VM = %+v, %v, want the pack's SHA-256", res, err)
	}

	// The pack is rebuilt; the existing VM keeps the digest of the pack
	// it was created from, which its label records.
	if err := os.WriteFile(f.pack, []byte("a rebuilt pack"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, f.ws.HostPath, "smolvm", "sortie-VM-1")
	for _, state := range []string{StateRunning, StateStopped} {
		vm := f.ownedVM("VM-1", state)
		vm.Labels[LabelPack] = "0123abcd"
		f.fake.setState(fakeState{Machines: []fakeMachine{vm}})
		res, err := f.env.Prepare(context.Background(), f.ws)
		if err != nil || res.CreatedNow || res.ImageDigest != "0123abcd" {
			t.Errorf("Prepare() of a %s VM = %+v, %v, want the digest its label records", state, res, err)
		}
	}
}

func TestToolChannel_IsTheMountedSocket(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	if _, err := f.env.Prepare(context.Background(), f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	got, err := f.env.ToolChannel(f.ws)
	if err != nil {
		t.Fatalf("ToolChannel() error = %v", err)
	}
	if want := filepath.Join(f.stateDir, "env", "VM-1", "tools.sock"); got.HostSocket != want || got.GuestPort != GuestToolPort {
		t.Errorf("ToolChannel() = %+v, want socket %q at guest port %d", got, want, GuestToolPort)
	}
	argvs := f.fake.argvs()
	if len(argvs) < 2 {
		t.Fatalf("calls = %q, want a create", argvs)
	}
	create := strings.Join(argvs[1], " ")
	if !strings.Contains(create, "--mount-socket "+got.HostSocket+":"+GuestToolSocket) {
		t.Errorf("create argv %q does not mount %q at %q", create, got.HostSocket, GuestToolSocket)
	}
	if _, err := f.env.ToolChannel(environment.Workspace{Key: "../x"}); err == nil {
		t.Error("ToolChannel(../x) succeeded, want an error")
	}
}

// A pack without socat cannot carry the tool channel into the VM, so
// preparation fails with an error that says so.
func TestPrepare_RefusesAPackWithoutSocat(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Fail: map[string]fakeFailure{"machine exec": {Exit: noSocatExit}}})
	_, err := f.env.Prepare(context.Background(), f.ws)
	if err == nil || !strings.Contains(err.Error(), "socat") {
		t.Fatalf("Prepare() error = %v, want a refusal naming socat", err)
	}
	execs := 0
	for _, argv := range f.fake.argvs() {
		if slices.Contains(argv, "-d") {
			t.Errorf("calls include %q, want no listener started without socat", argv)
		}
		if argv[1] == "exec" {
			execs++
		}
	}
	if execs != toolListenerAttempts {
		t.Errorf("socat checks = %d, want %d: a VM that just started can lack its root filesystem for a moment", execs, toolListenerAttempts)
	}
}

// A check that fails for another reason, as a guest still settling right
// after its start can, is retried and its last error reported.
func TestPrepare_RetriesAFailedToolListenerCheck(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, nil)
	f.fake.setState(fakeState{Fail: map[string]fakeFailure{"machine exec": {Exit: 127, Stderr: "Error: guest not ready"}}})
	_, err := f.env.Prepare(context.Background(), f.ws)
	if err == nil || strings.Contains(err.Error(), "has no socat") {
		t.Fatalf("Prepare() error = %v, want a check failure that does not blame socat", err)
	}
	execs := 0
	for _, argv := range f.fake.argvs() {
		if argv[1] == "exec" {
			execs++
		}
	}
	if execs != toolListenerAttempts {
		t.Errorf("socat checks = %d, want %d", execs, toolListenerAttempts)
	}
}
