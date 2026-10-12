package huly

import (
	"context"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
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
