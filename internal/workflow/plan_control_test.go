package workflow

import (
	"path/filepath"
	"strings"
	"testing"
)

const planControlWorkflow = `---
tracker:
  kind: file
  active_states: [Todo, In Progress]
  terminal_states: [Done, Canceled]
  release_state: Todo
  backlog_state: Backlog
  cancel_state: Canceled
  done_state: Done
reactions:
  merge_completion:
    provider: github
    target_state: Done
polling:
  interval_ms: %d
---
Do the task for {{ .issue.title }}.
`

// A reload that breaks plan control keeps the previous configuration.
func TestManager_ReloadKeepsPlanControlOnInvalidEdit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	mustWriteFile(t, path, []byte(strings.Replace(planControlWorkflow, "%d", "5000", 1)))

	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !mgr.Config().Tracker.PlanControl() {
		t.Fatal("plan control off after load, want on")
	}

	broken := strings.Replace(planControlWorkflow, "%d", "9999", 1)
	broken = strings.Replace(broken, "  cancel_state: Canceled\n", "", 1)
	mustWriteFile(t, path, []byte(broken))

	if err := mgr.Reload(); err == nil {
		t.Fatal("Reload() = nil, want the missing cancel_state error")
	}
	if got := mgr.Config().Polling.IntervalMS; got != 5000 {
		t.Errorf("after failed Reload: Polling.IntervalMS = %d, want 5000 (previous config kept)", got)
	}
	if got := mgr.Config().Tracker.CancelState; got != "Canceled" {
		t.Errorf("after failed Reload: CancelState = %q, want Canceled", got)
	}
	if err := mgr.LastLoadError(); err == nil || !strings.Contains(err.Error(), "tracker.cancel_state") {
		t.Errorf("LastLoadError() = %v, want it to name tracker.cancel_state", err)
	}
}

// A reload that adds a criteria rule without a criteria check keeps the
// previous configuration.
func TestManager_ReloadRefusesACriteriaChainWithoutTheGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	base := strings.Replace(planControlWorkflow, "%d", "5000", 1)
	mustWriteFile(t, path, []byte(base))
	mgr, err := NewManager(path, testLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	broken := strings.Replace(base, "polling:\n  interval_ms: 5000\n", `polling:
  interval_ms: 7000
dispatch:
  rules:
    - name: implement
      match:
        labels: [sortie-review:criteria]
      agent: mock
`, 1)
	mustWriteFile(t, path, []byte(broken))
	if err := mgr.Reload(); err == nil || !strings.Contains(err.Error(), "gate: criteria") {
		t.Fatalf("Reload() = %v, want the missing criteria gate", err)
	}
	if got := mgr.Config().Polling.IntervalMS; got != 5000 {
		t.Errorf("Polling.IntervalMS = %d, want 5000 (previous config kept)", got)
	}
}
