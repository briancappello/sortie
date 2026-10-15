package huly

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// newPlanProjectAdapter is an adapter on seedWorkspace configured for plan
// writes, with an account to make member of created projects.
func newPlanProjectAdapter(t *testing.T, f *fakeRPC) *HulyAdapter {
	t.Helper()
	f.add(classProject, map[string]any{"_id": "proj1", "identifier": "HULY", "name": "Huly", "type": "ptype1"})
	a := newTestAdapter(t, f)
	a.planNames = [2]string{"Epic", "Story"}
	a.accountUUID = "acct-1"
	return a
}

func seedPlanWorkspace() *fakeRPC {
	f := newFakeRPC()
	f.add(classProjectType, map[string]any{"_id": "ptype1", "name": "Classic project", "statuses": []map[string]any{
		{"_id": "st-backlog"}, {"_id": "st-todo"}, {"_id": "st-progress"}, {"_id": "st-done"}, {"_id": "st-canceled"},
	}})
	for id, name := range map[string]string{
		"st-backlog": "Backlog", "st-todo": "Todo", "st-progress": "In Progress", "st-done": "Done", "st-canceled": "Canceled",
	} {
		f.add(classIssueStatus, map[string]any{"_id": id, "name": name})
	}
	return f
}

func TestInspectPlanProjectReady(t *testing.T) {
	t.Parallel()

	f := seedPlanWorkspace()
	seedTaskTypes(f)
	a := newPlanProjectAdapter(t, f)

	got, err := a.InspectPlanProject(context.Background(), "HULY")
	if err != nil {
		t.Fatalf("InspectPlanProject() error = %v", err)
	}
	if !got.Ready() || got.Name != "Huly" || got.Created {
		t.Errorf("InspectPlanProject() = %+v, want an existing ready project", got)
	}
	if got.URL != "http://huly.test/workbench/ws/tracker/proj1/issues" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestInspectPlanProjectMissingIsNotAnError(t *testing.T) {
	t.Parallel()

	a := newPlanProjectAdapter(t, seedPlanWorkspace())
	got, err := a.InspectPlanProject(context.Background(), "BILL")
	if err != nil {
		t.Fatalf("InspectPlanProject() error = %v", err)
	}
	if got.Exists || got.Key != "BILL" {
		t.Errorf("InspectPlanProject() = %+v, want a missing BILL", got)
	}
}

func TestInspectPlanProjectRefusesMalformedKey(t *testing.T) {
	t.Parallel()

	a := newPlanProjectAdapter(t, seedPlanWorkspace())
	for _, key := range []string{"", "bill", "1AB", "TOOLONG", "B-1"} {
		if _, err := a.InspectPlanProject(context.Background(), key); trackerKind(err) != domain.ErrTrackerPayload {
			t.Errorf("InspectPlanProject(%q) error = %v, want payload error", key, err)
		}
	}
}

func TestInspectPlanProjectNamesWhatIsMissing(t *testing.T) {
	t.Parallel()

	f := seedPlanWorkspace()
	f.add(classTaskType, map[string]any{"_id": "tt-issue", "name": "Issue", "parent": "ptype1", "statuses": classicStatuses})
	f.add(classProject, map[string]any{"_id": "proj2", "identifier": "OTHER", "name": "Other", "type": "ptype1"})
	a := newPlanProjectAdapter(t, f)
	a.requiredStates = append(a.requiredStates, "Review")

	got, err := a.InspectPlanProject(context.Background(), "HULY")
	if err != nil {
		t.Fatalf("InspectPlanProject() error = %v", err)
	}
	if got.Ready() || len(got.Problems) != 3 {
		t.Fatalf("Problems = %q, want Epic, Story and the state", got.Problems)
	}
	all := strings.Join(got.Problems, "\n")
	for _, want := range []string{`task type "Epic"`, `task type "Story"`, "state(s) Review", `space type "Classic project"`, "all 2 projects"} {
		if !strings.Contains(all, want) {
			t.Errorf("Problems = %q, want them to mention %q", got.Problems, want)
		}
	}
}

func TestInspectPlanProjectWithoutPlanTypes(t *testing.T) {
	t.Parallel()

	f := seedPlanWorkspace()
	seedTaskTypes(f)
	a := newPlanProjectAdapter(t, f)
	a.planNames = [2]string{}

	got, err := a.InspectPlanProject(context.Background(), "HULY")
	if err != nil {
		t.Fatalf("InspectPlanProject() error = %v", err)
	}
	if len(got.Problems) != 1 || !strings.Contains(got.Problems[0], "huly.epic_type") {
		t.Errorf("Problems = %q, want the missing plan type settings", got.Problems)
	}
}

func TestEnsurePlanProjectCreatesOfTheConfiguredType(t *testing.T) {
	t.Parallel()

	f := seedPlanWorkspace()
	seedTaskTypes(f)
	a := newPlanProjectAdapter(t, f)

	got, err := a.EnsurePlanProject(context.Background(), "BILL", " Billing ")
	if err != nil {
		t.Fatalf("EnsurePlanProject() error = %v", err)
	}
	if !got.Created || !got.Ready() || got.Name != "Billing" {
		t.Errorf("EnsurePlanProject() = %+v, want a created ready project named Billing", got)
	}

	txs := f.txsOf(classProject)
	if len(txs) != 1 {
		t.Fatalf("project transactions = %d, want 1", len(txs))
	}
	attrs := txs[0]["attributes"].(map[string]any)
	if attrs["type"] != "ptype1" || attrs["identifier"] != "BILL" || txs[0]["objectSpace"] != spaceSpaces {
		t.Errorf("created project = %v in %v, want BILL of ptype1 in %s", attrs, txs[0]["objectSpace"], spaceSpaces)
	}
	if members := attrs["members"].([]string); !slices.Equal(members, []string{"acct-1"}) {
		t.Errorf("members = %v, want the caller's account", members)
	}
}

func TestEnsurePlanProjectLeavesAnExistingProjectAlone(t *testing.T) {
	t.Parallel()

	f := seedPlanWorkspace()
	a := newPlanProjectAdapter(t, f)

	got, err := a.EnsurePlanProject(context.Background(), "HULY", "Renamed")
	if err != nil {
		t.Fatalf("EnsurePlanProject() error = %v", err)
	}
	if got.Created || got.Name != "Huly" || len(f.txs) != 0 {
		t.Errorf("EnsurePlanProject() = %+v with %d writes, want the existing project unchanged", got, len(f.txs))
	}
	if got.Ready() {
		t.Errorf("Ready() = true, want the missing task types reported")
	}
}

func TestEnsurePlanProjectReportsAFailedWrite(t *testing.T) {
	t.Parallel()

	f := seedPlanWorkspace()
	a := newPlanProjectAdapter(t, f)
	f.txErr = &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "denied"}

	if _, err := a.EnsurePlanProject(context.Background(), "BILL", "Billing"); trackerKind(err) != domain.ErrTrackerAuth {
		t.Errorf("EnsurePlanProject() error = %v, want the write's auth error", err)
	}
}
