package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/plan"
	"github.com/sortie-ai/sortie/internal/registry"
)

const planGraphJSON = `{
  "schema_version": 1,
  "source_digest": "sha256:0000000000000000000000000000000000000000000000000000000000000001",
  "nodes": [
    {"plan_key": "billing", "kind": "epic", "change": "billing", "title": "billing", "body": "change", "approval": "approved", "has_spec": true, "depth": 0, "deliverable_kind": null, "covers": [], "acceptance_criteria": []},
    {"plan_key": "billing/invoice", "kind": "story", "change": "billing", "title": "Invoice", "body": "- ok", "approval": "approved", "has_spec": true, "depth": 0, "deliverable_kind": "story", "covers": [], "acceptance_criteria": ["ok"]},
    {"plan_key": "billing/tax", "kind": "story", "change": "billing", "title": "Tax", "body": "- ok", "approval": "approved", "has_spec": false, "depth": 1, "deliverable_kind": "enabler", "covers": [], "acceptance_criteria": ["ok"]}
  ],
  "parents": [{"child": "billing/invoice", "parent": "billing"}, {"child": "billing/tax", "parent": "billing"}],
  "blocks": [{"blocker": "billing/invoice", "blocked": "billing/tax"}],
  "unplanned_changes": []
}`

const planFileWorkflow = `---
tracker:
  kind: file
  active_states: [To Do]
  terminal_states: [Done]
agent:
  kind: mock
file:
  path: issues.json
---
Do {{ .issue.title }}.
`

func planFixture(t *testing.T, graph string) (graphPath, wfPath string) {
	t.Helper()
	dir := t.TempDir()
	graphPath = filepath.Join(dir, "graph.json")
	if err := os.WriteFile(graphPath, []byte(graph), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "issues.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	return graphPath, writeCustomWorkflowFile(t, dir, []byte(planFileWorkflow))
}

func runPlanCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPlanSyncDryRunAgainstFileTracker(t *testing.T) {
	graph, wf := planFixture(t, planGraphJSON)

	code, out, errOut := runPlanCLI("plan", "sync", "--graph", graph, "--dry-run", wf)
	if code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, errOut)
	}
	for _, want := range []string{
		"plan sync preview (tracker file):",
		"creates 3, updates 0, parents 0, relations added 1",
		`create story billing/tax "Tax" under billing`,
		"link billing/invoice blocks billing/tax",
		"missing tracker capabilities: create_issue, link_dependency",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPlanSyncApplyRefusedByFileTracker(t *testing.T) {
	graph, wf := planFixture(t, planGraphJSON)

	code, out, _ := runPlanCLI("plan", "sync", "--graph", graph, "--format", "json", wf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var report planSyncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Error == nil || report.Error.Kind != "tracker_unsupported_operation" || report.Result != nil {
		t.Errorf("report = %+v, want tracker_unsupported_operation and no result", report)
	}
	if len(report.Capabilities) != 0 {
		t.Errorf("capabilities = %v, want none for the file tracker", report.Capabilities)
	}
}

func TestPlanSyncRejectsMalformedGraphBeforeTracker(t *testing.T) {
	g := strings.Replace(planGraphJSON, `"schema_version": 1`, `"schema_version": 7`, 1)
	graph, _ := planFixture(t, g)

	// A workflow path that does not exist proves the tracker is never built.
	code, _, errOut := runPlanCLI("plan", "sync", "--graph", graph, "/nonexistent/WORKFLOW.md")
	if code != 1 || !strings.Contains(errOut, "schema_version 7") || !strings.Contains(errOut, "versions 1 to 2") {
		t.Errorf("exit %d, stderr %q; want a version error naming both versions", code, errOut)
	}
}

func TestPlanSyncUsageErrors(t *testing.T) {
	if code, _, errOut := runPlanCLI("plan", "sync"); code != 1 || !strings.Contains(errOut, "--graph is required") {
		t.Errorf("missing --graph: exit %d, %q", code, errOut)
	}
	if code, _, errOut := runPlanCLI("plan", "push"); code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("unknown command: exit %d, %q", code, errOut)
	}
	if code, out, _ := runPlanCLI("plan", "--help"); code != 0 || !strings.Contains(out, "sortie plan sync --graph FILE") {
		t.Errorf("help: exit %d, %q", code, out)
	}
}

var (
	_ domain.PlanKeyFinder    = (*writableTracker)(nil)
	_ domain.IssueCreator     = (*writableTracker)(nil)
	_ domain.IssueUpdater     = (*writableTracker)(nil)
	_ domain.IssueParenter    = (*writableTracker)(nil)
	_ domain.DependencyLinker = (*writableTracker)(nil)
)

// writableTracker is a minimal tracker with every write capability.
type writableTracker struct {
	domain.TrackerAdapter
	issues []domain.PlanIssue
	writes int
}

func (w *writableTracker) FindIssueByPlanKey(context.Context, string) (domain.PlanIssue, error) {
	return domain.PlanIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound}
}

func (w *writableTracker) ListPlanIssues(context.Context, bool) ([]domain.PlanIssue, error) {
	return append([]domain.PlanIssue{}, w.issues...), nil
}

func (w *writableTracker) CreateIssue(_ context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	w.writes++
	id := fmt.Sprintf("i%d", len(w.issues)+1)
	w.issues = append(w.issues, domain.PlanIssue{ID: id, Identifier: "T-" + id, PlanKey: in.PlanKey, Kind: in.Kind,
		Title: in.Title, BodyDigest: plan.BodyDigest(in.Body), ParentID: in.ParentID, BlockedBy: []string{}})
	return domain.CreatedIssue{ID: id, Identifier: "T-" + id}, nil
}

func (w *writableTracker) UpdateIssue(context.Context, string, domain.IssueFields) error {
	w.writes++
	return nil
}
func (w *writableTracker) SetParent(context.Context, string, string) error { w.writes++; return nil }
func (w *writableTracker) RemoveDependency(context.Context, string, string) error {
	w.writes++
	return nil
}

func (w *writableTracker) AddDependency(_ context.Context, blocker, blocked string) error {
	w.writes++
	for i := range w.issues {
		if w.issues[i].ID == blocked {
			w.issues[i].BlockedBy = append(w.issues[i].BlockedBy, blocker)
		}
	}
	return nil
}

func TestPlanSyncAppliesAndIsIdempotent(t *testing.T) {
	graph, wf := planFixture(t, planGraphJSON)
	fake := &writableTracker{}
	orig := newPlanTracker
	newPlanTracker = func(string) (plan.Tracker, string, func(), error) {
		return plan.Tracker{Adapter: fake, Meta: registry.TrackerMeta{
			WriteCapabilities: registry.AllWriteCapabilities(), PlanKeyStorage: registry.PlanKeyCustomField,
		}}, "fake", func() {}, nil
	}
	t.Cleanup(func() { newPlanTracker = orig })

	code, out, errOut := runPlanCLI("plan", "sync", "--graph", graph, wf)
	if code != 0 || !strings.Contains(out, "applied: created 3, updated 0, parents set 0, relations added 1") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	writes := fake.writes
	code, out, _ = runPlanCLI("plan", "sync", "--graph", graph, wf)
	if code != 0 || fake.writes != writes || !strings.Contains(out, "creates 0, updates 0, parents 0, relations added 0") {
		t.Errorf("second run: exit %d, %d writes\n%s", code, fake.writes-writes, out)
	}
}
