//go:build unix

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// smolvmWorkflow returns a workflow selecting the smolvm kind with the
// given binary, pack, and CPU count.
func smolvmWorkflow(binary, pack string, cpus int) []byte {
	return fmt.Appendf(nil, `---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
environment:
  kind: smolvm
  smolvm:
    binary: %q
    pack: %q
    cpus: %d
---
Do {{ .issue.title }}.
`, binary, pack, cpus)
}

// writeVersionScript writes an executable that prints "smolvm <version>"
// for its version command.
func writeVersionScript(t *testing.T, path, version string) {
	t.Helper()
	agenttest.WriteScript(t, filepath.Dir(path), filepath.Base(path), "echo 'smolvm "+version+"'\n")
}

func writeSmolvmFixture(t *testing.T, version string) (dir, binary, pack string) {
	t.Helper()
	dir = t.TempDir()
	binary = filepath.Join(dir, "smolvm")
	writeVersionScript(t, binary, version)
	pack = filepath.Join(dir, "base.smolmachine")
	if err := os.WriteFile(pack, []byte("pack"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, binary, pack
}

// Not parallel: boot sets up the process-wide logger and redaction list.
func TestBoot_RefusesUnsupportedSmolvmVersion(t *testing.T) {
	dir, binary, pack := writeSmolvmFixture(t, "1.30.0")
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, smolvmWorkflow(binary, pack, 2), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	_, code := boot(context.Background(), bootParams{args: []string{"--port", "0", path}, stdout: &stdout, stderr: &stderr})

	if code == 0 {
		t.Fatal("boot() exit code = 0, want startup to stop on an unsupported smolvm version")
	}
	if msg := stderr.String(); !strings.Contains(msg, "1.30.0") || !strings.Contains(msg, "1.26.x") {
		t.Errorf("stderr = %q, want it to name 1.30.0 and 1.26.x", msg)
	}
}

func TestWorkflowValidate_ReloadRefusesUnsupportedSmolvmVersion(t *testing.T) {
	t.Parallel()

	dir, binary, pack := writeSmolvmFixture(t, "1.26.1")
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, smolvmWorkflow(binary, pack, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := workflow.NewManager(path, slog.New(slog.DiscardHandler),
		workflow.WithValidateFunc(workflowValidate),
		workflow.WithAgentKindProbe(registry.Agents.Has),
		workflow.WithRetiredAgents(registry.RetiredAgentOf),
		workflow.WithAdvisoryFunc(workflowAdvisories))
	if err != nil {
		t.Fatalf("NewManager() error = %v, want the supported version accepted", err)
	}

	writeVersionScript(t, binary, "1.30.0")
	if err := os.WriteFile(path, smolvmWorkflow(binary, pack, 4), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadErr := mgr.Reload()

	if reloadErr == nil || !strings.Contains(reloadErr.Error(), "1.30.0") || !strings.Contains(reloadErr.Error(), "1.26.x") {
		t.Fatalf("Reload() error = %v, want a refusal naming 1.30.0 and 1.26.x", reloadErr)
	}
	if mgr.LastLoadError() == nil {
		t.Error("LastLoadError() = nil, want the rejected reload recorded")
	}
	if cpus := mgr.Config().Environment.Smolvm.CPUs; cpus != 2 {
		t.Errorf("environment.smolvm.cpus after a rejected reload = %d, want the previous 2", cpus)
	}
}
