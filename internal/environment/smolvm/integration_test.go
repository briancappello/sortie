//go:build unix

package smolvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/environment"
)

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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := cli.Delete(ctx, name); err != nil && !IsNotFound(err) {
			t.Errorf("delete VM %s: %v", name, err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
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
	// smolvm counts the two directory mounts; the socket is not counted.
	if m.Mounts != 2 || m.Network {
		t.Errorf("VM mounts = %d, network = %v; want 2 mounts and no network", m.Mounts, m.Network)
	}
	if err := guest("echo from-guest > /run/sortie/control/probe && test -S /run/sortie/tools.sock && test -d /opt/sortie"); err != nil {
		t.Fatalf("guest mount check: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(host, ".sortie", "probe")); err != nil || strings.TrimSpace(string(data)) != "from-guest" {
		t.Errorf("host .sortie/probe = %q, %v, want the guest's write", data, err)
	}
	if err := guest("touch /opt/sortie/planted"); err == nil {
		t.Error("guest write to /opt/sortie succeeded, want a read-only mount")
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
