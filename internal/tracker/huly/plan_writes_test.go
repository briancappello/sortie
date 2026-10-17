package huly

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tracker/huly/markup"
)

// newPlanAdapter returns an adapter over seedWorkspace with the Epic and
// Story task types resolved, project sequence 3, and ranked issues.
func newPlanAdapter(t *testing.T) (*HulyAdapter, *fakeRPC) {
	t.Helper()

	f := seedWorkspace()
	seedTaskTypes(f)
	f.mu.Lock()
	f.docs[classProject][0]["sequence"] = 3
	f.docs[classProject][0]["defaultIssueStatus"] = "st-todo"
	f.docs[classIssue][0]["rank"] = "0|hzzzzz:"
	f.docs[classIssue][1]["rank"] = "0|i00007:"
	f.mu.Unlock()
	a := newTestAdapter(t, f)
	if err := a.resolvePlanTypes(context.Background(), "Epic", "Story"); err != nil {
		t.Fatal(err)
	}
	return a, f
}

func issueDoc(f *fakeRPC, id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.docs[classIssue] {
		if d["_id"] == id {
			return d
		}
	}
	return nil
}

func TestCreateIssueTransactionShape(t *testing.T) {
	t.Parallel()

	a, f := newPlanAdapter(t)
	ctx := context.Background()

	epic, err := a.CreateIssue(ctx, domain.NewIssue{
		Title: "billing", Body: "## Why\n\nInvoices.", Kind: domain.IssueKindEpic, PlanKey: "billing",
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if epic.Identifier != "HULY-4" {
		t.Errorf("epic identifier = %s, want HULY-4 from sequence 3", epic.Identifier)
	}

	// The number comes from a $inc on the project's sequence with retrieve set.
	inc := f.txsOf(classProject)
	if len(inc) != 1 || inc[0]["retrieve"] != true || inc[0]["objectSpace"] != spaceSpace {
		t.Fatalf("sequence tx = %+v, want one retrieving update in core:space:Space", inc)
	}
	if ops := inc[0]["operations"].(map[string]any); ops["$inc"].(map[string]any)["sequence"] != 1 {
		t.Errorf("sequence operations = %v, want $inc sequence 1", ops)
	}

	creates := f.txsOf(classIssue)
	tx := creates[len(creates)-1]
	if tx["_class"] != classTxCreateDoc || tx["objectSpace"] != "proj1" ||
		tx["attachedTo"] != noParent || tx["attachedToClass"] != classIssue || tx["collection"] != collectionSubIssues {
		t.Errorf("epic tx = %+v, want a create under NoParent in the subIssues collection", tx)
	}
	attrs := tx["attributes"].(map[string]any)
	for k, want := range map[string]any{
		"title": "billing", "number": 4, "identifier": "HULY-4", "kind": "tt-epic", "status": "st-todo",
		"rank": "0|i0000f:", attrPlanKey: "billing", attrPlanBodyDigest: bodyDigest("## Why\n\nInvoices."),
	} {
		if attrs[k] != want {
			t.Errorf("epic attribute %s = %v, want %v", k, attrs[k], want)
		}
	}
	ref, _ := attrs["description"].(string)
	if got := markup.ToMarkdown(f.markup[ref]); got != "## Why\n\nInvoices." {
		t.Errorf("description = %q, want the body as markup", got)
	}

	story, err := a.CreateIssue(ctx, domain.NewIssue{
		Title: "Invoice", Kind: domain.IssueKindStory, PlanKey: "billing/invoice", ParentID: epic.ID,
	})
	if err != nil {
		t.Fatalf("CreateIssue(story) error = %v", err)
	}
	doc := issueDoc(f, story.ID)
	if doc["attachedTo"] != epic.ID || doc["collection"] != collectionSubIssues || doc["kind"] != "tt-story" {
		t.Errorf("story = %+v, want it in the epic's subIssues with the story type", doc)
	}
	parents, _ := doc["parents"].([]map[string]any)
	if len(parents) != 1 || parents[0]["parentId"] != epic.ID || parents[0]["identifier"] != "HULY-4" {
		t.Errorf("story parents = %v, want the epic", doc["parents"])
	}
	if doc["description"] != nil {
		t.Errorf("story description = %v, want none for an empty body", doc["description"])
	}
	if doc["rank"] != "0|i0000n:" || story.Identifier != "HULY-5" {
		t.Errorf("story rank %v identifier %s, want 0|i0000n: and HULY-5", doc["rank"], story.Identifier)
	}

	found, err := a.FindIssueByPlanKey(ctx, "billing/invoice")
	if err != nil || found.ID != story.ID || found.ParentID != epic.ID || found.Kind != domain.IssueKindStory {
		t.Errorf("FindIssueByPlanKey() = %+v, %v; want the story under the epic", found, err)
	}
}

func TestCreateIssueRefusals(t *testing.T) {
	t.Parallel()

	a, f := newPlanAdapter(t)
	ctx := context.Background()
	if _, err := a.CreateIssue(ctx, domain.NewIssue{Title: "x", Kind: domain.IssueKindEpic, PlanKey: "billing"}); err != nil {
		t.Fatal(err)
	}
	f.add(classIssue, map[string]any{"_id": "foreign", "space": "proj-other", "identifier": "GAME-1", "title": "f"})

	tests := []struct {
		name string
		in   domain.NewIssue
		want domain.TrackerErrorKind
	}{
		{"blank title", domain.NewIssue{Title: " ", Kind: domain.IssueKindEpic, PlanKey: "a"}, domain.ErrTrackerPayload},
		{"no plan key", domain.NewIssue{Title: "t", Kind: domain.IssueKindEpic}, domain.ErrTrackerPayload},
		{"unknown kind", domain.NewIssue{Title: "t", Kind: "bug", PlanKey: "a"}, domain.ErrTrackerPayload},
		{"duplicate plan key", domain.NewIssue{Title: "t", Kind: domain.IssueKindEpic, PlanKey: "billing"}, domain.ErrTrackerPayload},
		{"missing parent", domain.NewIssue{Title: "t", Kind: domain.IssueKindStory, PlanKey: "a/b", ParentID: "nope"}, domain.ErrTrackerNotFound},
		{"parent in another project", domain.NewIssue{Title: "t", Kind: domain.IssueKindStory, PlanKey: "a/b", ParentID: "foreign"}, domain.ErrTrackerNotFound},
	}
	before := len(f.txsOf(classIssue))
	for _, tt := range tests {
		if _, err := a.CreateIssue(ctx, tt.in); trackerKind(err) != tt.want {
			t.Errorf("%s: CreateIssue() error = %v, want %s", tt.name, err, tt.want)
		}
	}
	if got := len(f.txsOf(classIssue)); got != before {
		t.Errorf("refused creates wrote %d issue transactions", got-before)
	}
}

func TestCreateIssueWithoutTaskTypes(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)
	_, err := a.CreateIssue(context.Background(), domain.NewIssue{Title: "t", Kind: domain.IssueKindEpic, PlanKey: "a"})
	if trackerKind(err) != domain.ErrTrackerPayload || !strings.Contains(err.Error(), "huly.epic_type") {
		t.Errorf("CreateIssue() error = %v, want a payload error naming the config keys", err)
	}
	if len(f.txs) != 0 {
		t.Errorf("transactions = %d, want none", len(f.txs))
	}
}

func blockedByIDs(f *fakeRPC, id string) []string {
	var out []string
	for _, b := range toSlice(issueDoc(f, id)["blockedBy"]) {
		out = append(out, b.(map[string]any)["_id"].(string))
	}
	return out
}

func TestDependencyLinkerIdempotent(t *testing.T) {
	t.Parallel()

	a, f := newPlanAdapter(t)
	ctx := context.Background()

	// HULY-3 is not blocked yet; link HULY-2 as its blocker twice.
	for range 2 {
		if err := a.AddDependency(ctx, "HULY-2", "HULY-3"); err != nil {
			t.Fatalf("AddDependency() error = %v", err)
		}
	}
	if got := blockedByIDs(f, "issue3"); !slices.Equal(got, []string{"issue2"}) {
		t.Errorf("blockedBy = %v, want one relation to issue2", got)
	}
	if n := len(f.txsOf(classIssue)); n != 1 {
		t.Errorf("issue transactions = %d, want 1 (the second link writes nothing)", n)
	}
	push := f.txsOf(classIssue)[0]["operations"].(map[string]any)["$push"].(map[string]any)["blockedBy"].(map[string]any)
	if push["_id"] != "issue2" || push["_class"] != classIssue {
		t.Errorf("$push = %v, want the blocker ref", push)
	}

	// The candidate listing reports the new blocker.
	issues, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		if i.Identifier == "HULY-3" && (len(i.BlockedBy) != 1 || i.BlockedBy[0].Identifier != "HULY-2" || i.BlockedBy[0].State != "Done") {
			t.Errorf("HULY-3 blockers = %+v, want HULY-2 (Done)", i.BlockedBy)
		}
	}

	// Unlinking a relation that does not exist writes nothing.
	if err := a.RemoveDependency(ctx, "HULY-1", "HULY-3"); err != nil {
		t.Fatalf("RemoveDependency(missing) error = %v", err)
	}
	if n := len(f.txsOf(classIssue)); n != 1 {
		t.Errorf("issue transactions after a missing unlink = %d, want 1", n)
	}
	if err := a.RemoveDependency(ctx, "HULY-2", "HULY-3"); err != nil {
		t.Fatalf("RemoveDependency() error = %v", err)
	}
	if got := blockedByIDs(f, "issue3"); len(got) != 0 {
		t.Errorf("blockedBy after unlink = %v, want empty", got)
	}
}

func TestDependencyLinkerRefusals(t *testing.T) {
	t.Parallel()

	a, f := newPlanAdapter(t)
	f.add(classIssue, map[string]any{"_id": "foreign", "space": "proj-other", "identifier": "GAME-1", "title": "f"})
	ctx := context.Background()
	if err := a.AddDependency(ctx, "HULY-1", "HULY-1"); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("self link error = %v, want payload", err)
	}
	if err := a.AddDependency(ctx, "GAME-1", "HULY-1"); trackerKind(err) != domain.ErrTrackerNotFound {
		t.Errorf("link to another project error = %v, want not found", err)
	}
	if len(f.txs) != 0 {
		t.Errorf("refused links wrote %d transactions", len(f.txs))
	}
}

func TestUpdateIssue(t *testing.T) {
	t.Parallel()

	a, f := newPlanAdapter(t)
	ctx := context.Background()
	created, err := a.CreateIssue(ctx, domain.NewIssue{Title: "Old", Kind: domain.IssueKindStory, PlanKey: "b/x"})
	if err != nil {
		t.Fatal(err)
	}
	ptr := func(s string) *string { return &s }
	epic := domain.IssueKindEpic

	// No body yet: the first body creates the description.
	if err := a.UpdateIssue(ctx, created.ID, domain.IssueFields{Title: ptr("New"), Body: ptr("- one"), Kind: &epic}); err != nil {
		t.Fatalf("UpdateIssue() error = %v", err)
	}
	doc := issueDoc(f, created.ID)
	ref, _ := doc["description"].(string)
	if doc["title"] != "New" || doc["kind"] != "tt-epic" || doc[attrPlanBodyDigest] != bodyDigest("- one") ||
		markup.ToMarkdown(f.markup[ref]) != "- one" {
		t.Fatalf("issue = %+v, want the new title, kind, digest and description", doc)
	}

	// An existing description is replaced in place.
	if err := a.UpdateIssue(ctx, created.ID, domain.IssueFields{Body: ptr("- two")}); err != nil {
		t.Fatal(err)
	}
	if doc := issueDoc(f, created.ID); doc["description"] != ref || markup.ToMarkdown(f.markup[ref]) != "- two" {
		t.Errorf("description = %v %q, want ref %s updated to \"- two\"", doc["description"], markup.ToMarkdown(f.markup[ref]), ref)
	}

	// Unchanged values write nothing.
	before := len(f.txsOf(classIssue))
	if err := a.UpdateIssue(ctx, created.ID, domain.IssueFields{Title: ptr("New"), Body: ptr("- two"), Kind: &epic}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.txsOf(classIssue)); got != before {
		t.Errorf("an unchanged update wrote %d transactions", got-before)
	}
	if err := a.UpdateIssue(ctx, created.ID, domain.IssueFields{Title: ptr("  ")}); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("blank title error = %v, want payload", err)
	}
}

func TestSetParent(t *testing.T) {
	t.Parallel()

	a, f := newPlanAdapter(t)
	ctx := context.Background()

	// HULY-3 is under HULY-1; move it under HULY-2.
	if err := a.SetParent(ctx, "HULY-3", "HULY-2"); err != nil {
		t.Fatalf("SetParent() error = %v", err)
	}
	doc := issueDoc(f, "issue3")
	parents, _ := doc["parents"].([]map[string]any)
	if doc["attachedTo"] != "issue2" || len(parents) != 1 || parents[0]["parentId"] != "issue2" {
		t.Errorf("issue3 = %+v, want attachedTo and parents naming issue2", doc)
	}
	tx := f.txsOf(classIssue)[0]
	if tx["attachedTo"] != "issue1" || tx["collection"] != collectionSubIssues {
		t.Errorf("tx = %+v, want it filed in the old parent's subIssues", tx)
	}

	// Setting the same parent again writes nothing.
	if err := a.SetParent(ctx, "HULY-3", "issue2"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.txsOf(classIssue)); n != 1 {
		t.Errorf("transactions = %d, want 1", n)
	}

	// A cycle is refused; removing the parent moves it to the top level.
	if err := a.SetParent(ctx, "HULY-2", "HULY-3"); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("cycle error = %v, want payload", err)
	}
	if err := a.SetParent(ctx, "HULY-3", ""); err != nil {
		t.Fatal(err)
	}
	if doc := issueDoc(f, "issue3"); doc["attachedTo"] != noParent {
		t.Errorf("attachedTo = %v, want NoParent", doc["attachedTo"])
	}
	if err := a.SetParent(ctx, "HULY-2", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(f.txsOf(classIssue)); n != 2 {
		t.Errorf("transactions = %d, want 2 (unparenting a top-level issue writes nothing)", n)
	}
}

func TestRegistrationDeclaresWriteCapabilities(t *testing.T) {
	t.Parallel()

	meta, _ := registry.Trackers.Meta("huly")
	for _, c := range registry.AllWriteCapabilities() {
		if !meta.HasWriteCapability(c) {
			t.Errorf("huly does not declare %s", c)
		}
	}
	if meta.PlanKeyStorage != registry.PlanKeyCustomField {
		t.Errorf("PlanKeyStorage = %q, want custom_field", meta.PlanKeyStorage)
	}
}
