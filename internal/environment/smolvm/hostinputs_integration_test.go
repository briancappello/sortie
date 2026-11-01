//go:build unix

package smolvm

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
	"github.com/sortie-ai/sortie/internal/waittest"
)

// newIntegrationEnvWith returns a gated test's smolvm environment, its
// settings changed by mutate, which deletes the VM of every key it is
// told about when the test ends.
func newIntegrationEnvWith(t *testing.T, mutate func(*config.SmolvmConfig)) *integrationEnv {
	t.Helper()
	requireSmolvm(t)
	pack := integrationPack(t)
	if _, err := os.Stat(pack); err != nil {
		t.Fatalf("pack %s: %v; set SORTIE_SMOLVM_TEST_PACK", pack, err)
	}
	cfg := config.SmolvmConfig{Binary: DefaultBinary, Pack: pack}
	mutate(&cfg)
	root := t.TempDir()
	built, err := New(environment.Params{
		Config:       config.EnvironmentConfig{Kind: config.EnvironmentKindSmolvm, Smolvm: cfg},
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

// prepareNew prepares a fresh workspace with a key from prefix and
// returns it.
func (ie *integrationEnv) prepareNew(t *testing.T, prefix string) environment.Workspace {
	t.Helper()
	key := ie.newKey(prefix)
	host := filepath.Join(ie.root, key)
	if err := os.Mkdir(host, 0o750); err != nil {
		t.Fatal(err)
	}
	ws := environment.Workspace{Key: key, HostPath: host, GuestPath: GuestWorkspace, Identifier: key}
	if _, err := ie.env.Prepare(liveContext(t), ws); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	return ws
}

// requireNestedKVM skips the test on a host whose KVM module does not
// allow nested guests.
func requireNestedKVM(t *testing.T) {
	t.Helper()
	for _, module := range []string{"kvm_intel", "kvm_amd"} {
		data, err := os.ReadFile(filepath.Join("/sys/module", module, "parameters", "nested")) //nolint:gosec // G304: a fixed sysfs path
		if err == nil && (strings.TrimSpace(string(data)) == "Y" || strings.TrimSpace(string(data)) == "1") {
			return
		}
	}
	t.Skip("skipping the nested virtualization test: the host's KVM module does not allow nested guests (kvm_intel or kvm_amd nested=1)")
}

// buildKVMProbe builds testdata/kvmprobe as a static binary into a new
// directory and returns the directory.
func buildKVMProbe(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the nested virtualization test builds its guest probe with go: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: the guest user runs the probe
		t.Fatal(err)
	}
	cmd := exec.CommandContext(liveContext(t), gobin, "build", "-o", filepath.Join(dir, "kvmprobe"), "./testdata/kvmprobe") //nolint:gosec // G204: the test's own probe
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the KVM probe: %v\n%s", err, out)
	}
	return dir
}

// A workflow that sets nested gets VMs that can start a KVM guest of
// their own.
func TestIntegration_NestedVMCanStartAKVMGuest(t *testing.T) {
	requireSmolvm(t)
	requireNestedKVM(t)
	probe := buildKVMProbe(t)
	ie := newIntegrationEnvWith(t, func(cfg *config.SmolvmConfig) {
		cfg.Nested = true
		cfg.Mounts = []config.SmolvmMount{{Host: probe, Guest: "/opt/kvmprobe", ReadOnly: true}}
	})
	ws := ie.prepareNew(t, "SMOLNEST")

	out, err := ie.guest(liveContext(t), ws.Key, "test -c /dev/kvm && /opt/kvmprobe/kvmprobe")
	if err != nil || !strings.Contains(out, "KVM_CREATE_VM ok") {
		t.Errorf("KVM probe in the nested VM = %q, %v, want /dev/kvm and a created VM", out, err)
	}
}

// With net_backend virtio-net and an allowlist, the VM reaches a listed
// host and no other.
func TestIntegration_NetBackendKeepsTheAllowlist(t *testing.T) {
	ie := newIntegrationEnvWith(t, func(cfg *config.SmolvmConfig) {
		cfg.NetBackend = config.SmolvmNetBackendVirtioNet
		cfg.AllowHosts = []string{"ghcr.io"}
	})
	ws := ie.prepareNew(t, "SMOLNET")
	ctx := liveContext(t)

	if m, ok := ie.listed(ctx, t, VMName(ws.Key)); !ok || !m.Network {
		t.Fatalf("VM = %+v (listed %v), want it listed with its network", m, ok)
	}
	code, err := ie.guest(ctx, ws.Key, `curl -sS -o /dev/null -m 30 -w '%{http_code}' https://ghcr.io/`)
	if err != nil || strings.TrimSpace(code) == "000" {
		t.Errorf("request to the listed ghcr.io = %q, %v, want an HTTP response", code, err)
	}
	if out, err := ie.guest(ctx, ws.Key, `curl -sS -o /dev/null -m 15 https://example.com/`); err == nil {
		t.Errorf("request to the unlisted example.com = %q, nil, want it refused", out)
	}
}

// A read-only mount shows the host's snapshot in the guest, and refuses
// the guest's writes to it.
func TestIntegration_ReadOnlyMount(t *testing.T) {
	requireSmolvm(t)
	sources := t.TempDir()
	// The guest user need not be the host user.
	if err := os.Chmod(sources, 0o755); err != nil { //nolint:gosec // G302: the guest user reads it
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sources, "a.txt"), []byte("snapshot\n"), 0o644); err != nil { //nolint:gosec // G306: the guest user reads it
		t.Fatal(err)
	}
	const guestPath = "/home/agent/.cache/homelab-sources"
	ie := newIntegrationEnvWith(t, func(cfg *config.SmolvmConfig) {
		cfg.Mounts = []config.SmolvmMount{{Host: sources, Guest: guestPath, ReadOnly: true}}
	})
	ws := ie.prepareNew(t, "SMOLRO")
	ctx := liveContext(t)

	if out, err := ie.guest(ctx, ws.Key, "cat "+guestPath+"/a.txt"); err != nil || strings.TrimSpace(out) != "snapshot" {
		t.Errorf("guest read of the snapshot = %q, %v, want the host's content", out, err)
	}
	for _, write := range []string{"echo x > " + guestPath + "/a.txt", "touch " + guestPath + "/new", "rm " + guestPath + "/a.txt"} {
		if out, err := ie.guest(ctx, ws.Key, write); err == nil {
			t.Errorf("guest %q = %q, nil, want a read-only refusal", write, out)
		}
	}
	if data, err := os.ReadFile(filepath.Join(sources, "a.txt")); err != nil || string(data) != "snapshot\n" {
		t.Errorf("host snapshot after the guest's writes = %q, %v, want it unchanged", data, err)
	}
}

// What the guest wrote in its per-workspace directory is still on the
// host after its VM is killed mid-run and its workspace is removed.
func TestIntegration_EvidenceAfterACrash(t *testing.T) {
	requireSmolvm(t)
	recorder := filepath.Join(t.TempDir(), "sortie-recorder")
	ie := newIntegrationEnvWith(t, func(cfg *config.SmolvmConfig) {
		cfg.Mounts = []config.SmolvmMount{{Host: filepath.Join(recorder, config.SmolvmWorkspacePlaceholder), Guest: "/var/recorder"}}
	})
	ws := ie.prepareNew(t, "SMOLREC")
	ctx := liveContext(t)
	name := VMName(ws.Key)

	if _, err := ie.guest(ctx, ws.Key, "echo before-kill > /var/recorder/crash.txt"); err != nil {
		t.Fatalf("guest write to the per-workspace directory: %v", err)
	}
	m, ok := ie.listed(ctx, t, name)
	if !ok || m.PID == nil {
		t.Fatalf("VM = %+v (listed %v), want it running with a PID", m, ok)
	}
	if err := syscall.Kill(*m.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the VM's process %d: %v", *m.PID, err)
	}
	if err := waittest.UntilWithin(smolvmLiveBound, time.Second, func() error {
		if m, ok := ie.listed(ctx, t, name); ok && m.Running() {
			return errors.New("the killed VM is still listed as running")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := ie.env.Remove(ctx, ws); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := os.RemoveAll(ws.HostPath); err != nil {
		t.Fatal(err)
	}

	kept := filepath.Join(recorder, ws.Key)
	if data, err := os.ReadFile(filepath.Join(kept, "crash.txt")); err != nil || strings.TrimSpace(string(data)) != "before-kill" {
		t.Errorf("%s/crash.txt after the crash and the removal = %q, %v, want what the guest wrote", kept, data, err)
	}
	if info, err := os.Stat(kept); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("Stat(%s) = %v, %v, want the directory kept at mode 0700", kept, info, err)
	}
	if _, ok := ie.listed(ctx, t, name); ok {
		t.Errorf("VM %s is still listed after Remove()", name)
	}
}
