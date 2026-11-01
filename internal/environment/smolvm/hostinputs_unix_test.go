//go:build unix

package smolvm

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

// workflowSmolvmConfig parses the environment.smolvm block of a
// workflow's front matter, with the fixture's pack, as Sortie does.
func workflowSmolvmConfig(t *testing.T, pack string, smolvm map[string]any) config.SmolvmConfig {
	t.Helper()
	block := map[string]any{"pack": pack}
	maps.Copy(block, smolvm)
	cfg, err := config.NewServiceConfig(map[string]any{
		"environment": map[string]any{"kind": "smolvm", "smolvm": block},
	})
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	return cfg.Environment.Smolvm
}

// createArgv prepares the fixture's workspace and returns the create
// invocation's argv.
func createArgv(t *testing.T, f prepareFixture) []string {
	t.Helper()
	if _, err := f.env.Prepare(context.Background(), f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, argv := range f.fake.argvs() {
		if len(argv) >= 2 && argv[0] == "machine" && argv[1] == "create" {
			return argv
		}
	}
	t.Fatalf("calls = %q, want a create", f.fake.argvs())
	return nil
}

// A workflow that sets none of nested, net_backend and mounts creates
// its VMs with the argv Sortie used before they existed, written out
// here as it was.
func TestPrepare_NoHostInputsKeepsArgv(t *testing.T) {
	t.Parallel()

	for name, extra := range map[string]map[string]any{
		"absent": {},
		"off":    {"nested": false, "mounts": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
				smolvm := map[string]any{
					"cpus": 2, "memory_mib": 4096, "storage_gib": 20,
					"allow_hosts": []any{"models.opencode.ai", "*.npmjs.org"},
					"credentials": []any{
						map[string]any{"name": "openrouter", "env": "OPENROUTER_API_KEY", "hosts": []any{"openrouter.ai"}, "header": "X-Api-Key"},
					},
					"ssh_agent": true,
				}
				maps.Copy(smolvm, extra)
				parsed := workflowSmolvmConfig(t, cfg.Pack, smolvm)
				parsed.Binary = cfg.Binary
				*cfg = parsed
			})

			control := filepath.Join(f.ws.HostPath, ".sortie")
			socket := filepath.Join(f.stateDir, "env", "VM-1", "tools.sock")
			before := []string{
				"machine", "create", "--error-format", "json", "--name", "sortie-VM-1", "--from", f.pack,
				"--cpus", "2", "--mem", "4096", "--storage", "20",
				"--label", "sortie.managed=1",
				"--label", "sortie.workspace=VM-1",
				"--label", "sortie.pack=" + sha256Hex(testPackContent),
				"--label", "sortie.workflow=" + shortHash(f.workflow),
				"--allow-host-pattern", "models.opencode.ai",
				"--allow-host-pattern", "*.npmjs.org",
				"--allow-host-pattern", "openrouter.ai",
				"--credential", "openrouter=OPENROUTER_API_KEY@openrouter.ai:set-header=X-Api-Key",
				"--ssh-agent",
				"-v", control + ":/run/sortie/control",
				"--mount-socket", socket + ":/run/sortie/tools.sock",
			}
			got := createArgv(t, f)
			if !slices.Equal(got, before) {
				t.Errorf("create argv:\n got %q\nwant %q", got, before)
			}
			assertOnlyGrantedResources(t, got)
		})
	}
}

func TestPrepare_NestedIsAWorkflowOptIn(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) { cfg.Nested = true })
	on := createArgv(t, f)

	// The same workspace without nested: the argv it would be created with.
	f.env.cfg.Nested = false
	paths, err := f.env.preparePaths(f.ws)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := f.env.createSpec(VMName(f.ws.Key), f.ws, paths)
	if err != nil {
		t.Fatal(err)
	}
	off, err := createArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(off, "--nested") {
		t.Errorf("create argv without nested = %q, want no --nested", off)
	}
	if n := countArg(on, "--nested"); n != 1 {
		t.Errorf("create argv with nested = %q, want --nested once", on)
	}
	if without := slices.DeleteFunc(slices.Clone(on), func(a string) bool { return a == "--nested" }); !slices.Equal(without, off) {
		t.Errorf("create argv with nested less --nested = %q, want the argv without it %q", without, off)
	}
}

func TestPrepare_NetBackendWithAnAllowlist(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		parsed := workflowSmolvmConfig(t, cfg.Pack, map[string]any{"net_backend": "virtio-net", "allow_hosts": []any{"ghcr.io"}})
		parsed.Binary = cfg.Binary
		*cfg = parsed
	})
	create := strings.Join(createArgv(t, f), " ")
	if !strings.Contains(create, "--net-backend virtio-net --allow-host-pattern ghcr.io") {
		t.Errorf("create argv = %q, want the backend and the allowlist", create)
	}
	if strings.Contains(create, "--net ") || strings.HasSuffix(create, "--net") {
		t.Errorf("create argv = %q, want no --net: the allowlist alone opens the network", create)
	}
	if m := f.fake.state().Machines; len(m) != 1 || !m[0].Network {
		t.Errorf("machines = %+v, want one VM with its filtered network", m)
	}
}

// A backend that would leave the VM's egress unfiltered never reaches
// smolvm, whatever the configuration validation let through.
func TestPrepare_RefusesABackendWithoutFilteredEgress(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*config.SmolvmConfig){
		"tsi":           func(cfg *config.SmolvmConfig) { cfg.NetBackend = "tsi"; cfg.AllowHosts = []string{"ghcr.io"} },
		"no allowlist":  func(cfg *config.SmolvmConfig) { cfg.NetBackend = config.SmolvmNetBackendVirtioNet },
		"unknown value": func(cfg *config.SmolvmConfig) { cfg.NetBackend = "slirp"; cfg.AllowHosts = []string{"ghcr.io"} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPrepareFixture(t, fakeState{}, mutate)
			_, err := f.env.Prepare(context.Background(), f.ws)
			var ce *config.ConfigError
			if !errors.As(err, &ce) || ce.Field != "environment.smolvm.net_backend" {
				t.Fatalf("Prepare() error = %v, want a *ConfigError on environment.smolvm.net_backend", err)
			}
			assertArgvs(t, f, listArgv)
		})
	}
}

func TestPrepare_MountsTheWorkflowsDirectories(t *testing.T) {
	t.Parallel()

	sources := t.TempDir()
	recorder := filepath.Join(t.TempDir(), "sortie-recorder")
	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.Mounts = []config.SmolvmMount{
			{Host: sources, Guest: "/home/agent/.cache/homelab-sources", ReadOnly: true},
			{Host: filepath.Join(recorder, config.SmolvmWorkspacePlaceholder), Guest: "/var/recorder"},
		}
	})
	control := filepath.Join(f.ws.HostPath, ".sortie")
	create := createArgv(t, f)

	var volumes []string
	for i, arg := range create {
		if arg == "-v" && i+1 < len(create) {
			volumes = append(volumes, create[i+1])
		}
	}
	want := []string{
		control + ":/run/sortie/control",
		sources + ":/home/agent/.cache/homelab-sources:ro",
		filepath.Join(recorder, "VM-1") + ":/var/recorder",
	}
	if !slices.Equal(volumes, want) {
		t.Errorf("create volumes = %q, want %q", volumes, want)
	}
	if n := countArg(create, "--mount-socket"); n != 1 {
		t.Errorf("create argv has %d --mount-socket, want 1", n)
	}
}

func TestPrepare_CreatesThePerWorkspaceDirectoryBeforeTheVM(t *testing.T) {
	t.Parallel()

	recorder := filepath.Join(t.TempDir(), "state", "sortie-recorder")
	f := newPrepareFixture(t, fakeState{
		Fail: map[string]fakeFailure{"machine create": {Exit: 1, Stderr: `{"error":{"code":"CONFIG_ERROR","message":"refused"}}` + "\n"}},
	}, func(cfg *config.SmolvmConfig) {
		cfg.Mounts = []config.SmolvmMount{{Host: filepath.Join(recorder, config.SmolvmWorkspacePlaceholder), Guest: "/var/recorder"}}
	})
	if _, err := f.env.Prepare(context.Background(), f.ws); err == nil {
		t.Fatal("Prepare() error = nil, want the scripted create failure")
	}
	info, err := os.Stat(filepath.Join(recorder, "VM-1"))
	if err != nil || !info.IsDir() {
		t.Fatalf("Stat(per-workspace directory) = %v, %v, want a directory created before the create", info, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("per-workspace directory mode = %v, want 0700", perm)
	}
}

// What the guest wrote in its per-workspace directory survives the VM
// being killed, deleted, and recreated, and its workspace's removal.
func TestKeptDirs_EvidenceOutlivesTheVMAndTheWorkspace(t *testing.T) {
	t.Parallel()

	recorder := filepath.Join(t.TempDir(), "sortie-recorder")
	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.Mounts = []config.SmolvmMount{{Host: filepath.Join(recorder, config.SmolvmWorkspacePlaceholder), Guest: "/var/recorder"}}
	})
	f.ws = environment.Workspace{Key: "ONS-30", HostPath: filepath.Join(filepath.Dir(f.ws.HostPath), "ONS-30"), GuestPath: GuestWorkspace, Identifier: "ONS-30"}
	if err := os.MkdirAll(f.ws.HostPath, 0o750); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(recorder, "ONS-30")
	if got := f.env.KeptDirs(f.ws); !slices.Equal(got, []string{kept}) {
		t.Fatalf("KeptDirs() = %q, want [%s]", got, kept)
	}
	ctx := context.Background()
	if _, err := f.env.Prepare(ctx, f.ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	evidence := filepath.Join(kept, "crash.txt")
	if err := os.WriteFile(evidence, []byte("before-kill\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The VM is killed and smolvm lists it stopped; it is released,
	// recreated for a fresh workspace directory, then removed with its
	// workspace.
	state := f.fake.state()
	state.Machines[0].State, state.Machines[0].PID = StateStopped, nil
	f.fake.setState(state)
	if err := f.env.Release(ctx, f.ws); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if _, err := f.env.Prepare(ctx, f.ws); err != nil {
		t.Fatalf("Prepare() recreating the VM error = %v", err)
	}
	if err := f.env.Remove(ctx, f.ws); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := os.RemoveAll(f.ws.HostPath); err != nil {
		t.Fatal(err)
	}
	if err := f.env.Sweep(ctx, environment.SweepParams{WorkspaceRoot: filepath.Dir(f.ws.HostPath)}); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}

	data, err := os.ReadFile(evidence)
	if err != nil || string(data) != "before-kill\n" {
		t.Errorf("evidence after the workspace's removal = %q, %v, want what the guest wrote", data, err)
	}
	if info, err := os.Stat(kept); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("Stat(%s) = %v, %v, want the directory kept at mode 0700", kept, info, err)
	}
}

func TestKeptDirs(t *testing.T) {
	t.Parallel()

	f := newPrepareFixture(t, fakeState{}, func(cfg *config.SmolvmConfig) {
		cfg.Mounts = []config.SmolvmMount{
			{Host: "/srv/sources", Guest: "/src", ReadOnly: true},
			{Host: "/srv/recorder/{workspace}", Guest: "/var/recorder"},
			{Host: "/srv/out/{workspace}/logs", Guest: "/var/log/run"},
		}
	})
	if got, want := f.env.KeptDirs(f.ws), []string{"/srv/recorder/VM-1", "/srv/out/VM-1/logs"}; !slices.Equal(got, want) {
		t.Errorf("KeptDirs() = %q, want %q", got, want)
	}
	for _, key := range []string{"", "..", "a/b"} {
		if got := f.env.KeptDirs(environment.Workspace{Key: key}); got != nil {
			t.Errorf("KeptDirs(key %q) = %q, want none for a key that cannot name a directory", key, got)
		}
	}
	if got := newPrepareFixture(t, fakeState{}, nil).env.KeptDirs(f.ws); got != nil {
		t.Errorf("KeptDirs() without mounts = %q, want none", got)
	}
}

func countArg(argv []string, arg string) int {
	n := 0
	for _, a := range argv {
		if a == arg {
			n++
		}
	}
	return n
}
