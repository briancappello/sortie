package huly

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

var classicStatuses = []string{"st-backlog", "st-todo", "st-progress", "st-done", "st-canceled"}

// seedTaskTypes adds the Issue, Epic and Story task types to project type
// ptype1 of seedWorkspace.
func seedTaskTypes(f *fakeRPC) {
	for id, name := range map[string]string{"tt-issue": "Issue", "tt-epic": "Epic", "tt-story": "Story"} {
		f.add(classTaskType, map[string]any{"_id": id, "name": name, "parent": "ptype1", "statuses": classicStatuses})
	}
}

func TestResolvePlanTypes(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	seedTaskTypes(f)
	a := newTestAdapter(t, f)

	if err := a.resolvePlanTypes(context.Background(), "epic", " Story "); err != nil {
		t.Fatalf("resolvePlanTypes() error = %v", err)
	}
	if a.plan.epic.ID != "tt-epic" || a.plan.story.ID != "tt-story" {
		t.Errorf("plan types = %+v, want tt-epic and tt-story", a.plan)
	}
	if len(a.plan.story.Statuses) != len(classicStatuses) {
		t.Errorf("story statuses = %v, want the type's statuses", a.plan.story.Statuses)
	}
	if got := a.plan.kindOf("tt-story"); got != domain.IssueKindStory {
		t.Errorf("kindOf(tt-story) = %q, want story", got)
	}
	if got := a.plan.kindOf("tt-issue"); got != "" {
		t.Errorf("kindOf(tt-issue) = %q, want empty", got)
	}
}

func TestResolvePlanTypesMissingNamesProjectTypes(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	f.add(classTaskType, map[string]any{"_id": "tt-issue", "name": "Issue", "parent": "ptype1", "statuses": classicStatuses})
	f.add(classTaskType, map[string]any{"_id": "tt-other", "name": "Feature", "parent": "ptype-other", "statuses": classicStatuses})
	a := newTestAdapter(t, f)

	err := a.resolvePlanTypes(context.Background(), "Epic", "Story")
	if trackerKind(err) != domain.ErrTrackerPayload {
		t.Fatalf("resolvePlanTypes() error = %v, want payload error", err)
	}
	for _, want := range []string{`"Epic" (huly.epic_type)`, `"Story" (huly.story_type)`, "its task types are Issue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Feature") {
		t.Errorf("error %q names a task type of another project type", err)
	}
}

func TestResolvePlanTypesUnconfigured(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, seedWorkspace())
	if err := a.resolvePlanTypes(context.Background(), "", ""); err != nil {
		t.Fatalf("resolvePlanTypes() error = %v, want nil when neither is configured", err)
	}
	if a.plan.configured() {
		t.Error("plan types configured without config")
	}
	if _, err := a.plan.forKind(domain.IssueKindEpic); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("forKind() error = %v, want payload error naming the config keys", err)
	}
}

func TestResolvePlanTypesNeedsBoth(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	seedTaskTypes(f)
	a := newTestAdapter(t, f)
	for _, tc := range [][2]string{{"Epic", ""}, {"", "Story"}, {"Epic", "epic"}} {
		if err := a.resolvePlanTypes(context.Background(), tc[0], tc[1]); trackerKind(err) != domain.ErrTrackerPayload {
			t.Errorf("resolvePlanTypes(%q, %q) error = %v, want payload error", tc[0], tc[1], err)
		}
	}
}

func attributeNames(f *fakeRPC) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, d := range f.docs[classAttribute] {
		names = append(names, d["name"].(string))
	}
	return names
}

func TestEnsurePlanAttributesCreatesOnce(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)
	ctx := context.Background()

	if err := a.ensurePlanAttributes(ctx); err != nil {
		t.Fatalf("ensurePlanAttributes() error = %v", err)
	}
	want := []string{attrPlanKey, attrPlanBodyDigest, attrApprovedBy, attrApprovedByID, attrApprovedAt}
	if got := attributeNames(f); !slices.Equal(got, want) {
		t.Fatalf("attributes = %v, want %v", got, want)
	}
	created := f.txsOf(classAttribute)
	attrs := created[0]["attributes"].(map[string]any)
	if attrs["attributeOf"] != classIssue || attrs["isCustom"] != true || created[0]["objectSpace"] != spaceModel {
		t.Errorf("attribute tx = %+v, want a custom attribute of the Issue class in the model space", created[0])
	}

	// A second call, and a fresh adapter over the same workspace, create nothing.
	if err := a.ensurePlanAttributes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := newTestAdapter(t, f).ensurePlanAttributes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(f.txsOf(classAttribute)); got != len(want) {
		t.Errorf("attribute transactions = %d, want %d", got, len(want))
	}
}

// A workspace that has only the two attributes of earlier versions gets
// the approval attributes on its next plan write, and keeps the others.
func TestEnsurePlanAttributesAddsTheApprovalAttributes(t *testing.T) {
	t.Parallel()
	f := seedWorkspace()
	for _, name := range []string{attrPlanKey, attrPlanBodyDigest} {
		f.add(classAttribute, map[string]any{"_id": "old-" + name, "attributeOf": classIssue, "name": name})
	}
	if err := newTestAdapter(t, f).ensurePlanAttributes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.txsOf(classAttribute)); got != 3 {
		t.Errorf("attribute transactions = %d, want 3 (the approval attributes only)", got)
	}
}

func TestSetApproval(t *testing.T) {
	t.Parallel()
	f := seedWorkspace()
	a := newTestAdapter(t, f)
	ctx := context.Background()
	ap := domain.PlanApproval{By: "Ana Ruiz", ByID: "https://id.example|ana", At: "2026-10-16T14:02:00Z"}
	if err := a.SetApproval(ctx, "issue1", ap); err != nil {
		t.Fatalf("SetApproval() error = %v", err)
	}
	f.docs[classIssue][0][attrPlanKey] = "billing/x"
	got, err := a.FindIssueByPlanKey(ctx, "billing/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Approval == nil || *got.Approval != ap {
		t.Errorf("Approval = %+v, want %+v", got.Approval, ap)
	}
	if err := a.SetApproval(ctx, "nope", ap); trackerKind(err) != domain.ErrTrackerNotFound {
		t.Errorf("SetApproval(unknown) error = %v, want not found", err)
	}
}

func TestPlanKeyFinder(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	seedTaskTypes(f)
	f.add(classIssue, map[string]any{
		"_id": "issue9", "space": "proj1", "identifier": "HULY-9", "title": "Invoice", "status": "st-todo",
		"attachedTo": "issue1", "kind": "tt-story", attrPlanKey: "billing/invoice", attrPlanBodyDigest: "abc",
		"blockedBy": []map[string]any{{"_id": "issue2", "_class": classIssue}}, "modifiedOn": 1700000000000,
	})
	f.add(classIssue, map[string]any{
		"_id": "other", "space": "proj-other", "identifier": "GAME-1", "title": "Elsewhere", "status": "st-todo",
		attrPlanKey: "billing/tax", "modifiedOn": 1700000000000,
	})
	a := newTestAdapter(t, f)
	if err := a.resolvePlanTypes(context.Background(), "Epic", "Story"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := a.FindIssueByPlanKey(ctx, "billing/invoice")
	if err != nil {
		t.Fatalf("FindIssueByPlanKey() error = %v", err)
	}
	want := domain.PlanIssue{
		ID: "issue9", Identifier: "HULY-9", PlanKey: "billing/invoice", Kind: domain.IssueKindStory,
		Title: "Invoice", BodyDigest: "abc", State: "Todo", URL: "http://huly.test/workbench/ws/tracker/HULY-9",
		ParentID: "issue1", BlockedBy: []string{"issue2"}, Labels: []string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindIssueByPlanKey() = %+v, want %+v", got, want)
	}

	// The key in another project is not found.
	if _, err := a.FindIssueByPlanKey(ctx, "billing/tax"); trackerKind(err) != domain.ErrTrackerNotFound {
		t.Errorf("FindIssueByPlanKey(other project) error = %v, want not found", err)
	}

	keyed, err := a.ListPlanIssues(ctx, false)
	if err != nil || len(keyed) != 1 || keyed[0].PlanKey != "billing/invoice" {
		t.Fatalf("ListPlanIssues(false) = %+v, %v; want only HULY-9", keyed, err)
	}
	all, err := a.ListPlanIssues(ctx, true)
	if err != nil || len(all) != 4 {
		t.Fatalf("ListPlanIssues(true) = %d issues, %v; want 4", len(all), err)
	}
	for _, p := range all {
		if p.BlockedBy == nil {
			t.Errorf("%s BlockedBy is nil, want non-nil", p.Identifier)
		}
		if p.Identifier == "HULY-2" && (p.PlanKey != "" || p.ParentID != "" || p.Kind != "") {
			t.Errorf("unkeyed HULY-2 = %+v, want no plan key, parent, or kind", p)
		}
	}
}

func TestListPlanIssuesNeverPartial(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)
	f.findErr = &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "down"}
	got, err := a.ListPlanIssues(context.Background(), false)
	if trackerKind(err) != domain.ErrTrackerTransport || got != nil {
		t.Errorf("ListPlanIssues() = %v, %v; want nil and a transport error", got, err)
	}
}
