package plan

import (
	"context"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// firstPlan is the plan-sync spec's first sync: accounts-api with three
// approved deliverables and billing with two, billing after accounts-api.
func firstPlan() *graphBuilder {
	return newGraph().
		epic("accounts-api", ApprovalApproved, "open", "close", "audit").
		epic("billing", ApprovalApproved, "invoice", "tax").
		block("accounts-api", "billing").
		block("accounts-api/close", "billing/invoice").
		block("billing/invoice", "billing/tax")
}

// seedMatching makes the fake tracker hold exactly the graph's approved
// nodes, parents, and relations, as a completed sync leaves it.
func seedMatching(f *fakeTracker, g Graph) {
	ids := map[string]string{}
	for _, n := range g.Nodes {
		if n.Approval != ApprovalApproved {
			continue
		}
		f.seq++
		id := "seed-" + n.PlanKey
		ids[n.PlanKey] = id
		f.issues = append(f.issues, &domain.PlanIssue{
			ID: id, Identifier: n.PlanKey, PlanKey: n.PlanKey, Kind: domain.IssueKind(n.Kind),
			Title: n.Title, BodyDigest: BodyDigest(n.Body), BlockedBy: []string{},
		})
	}
	for _, e := range g.Parents {
		if c := f.byID(ids[e.Child]); c != nil && ids[e.Parent] != "" {
			c.ParentID = ids[e.Parent]
		}
	}
	for _, e := range g.Blocks {
		if b := f.byID(ids[e.Blocked]); b != nil && ids[e.Blocker] != "" {
			b.BlockedBy = append(b.BlockedBy, ids[e.Blocker])
		}
	}
}

func preview(t *testing.T, g Graph, tr Tracker) Preview {
	t.Helper()
	p, err := ComputePreview(context.Background(), g, tr)
	if err != nil {
		t.Fatalf("ComputePreview() error = %v", err)
	}
	return p
}

func TestPreviewFirstSync(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	p := preview(t, firstPlan().build(), f.tracker())

	if p.Counts != (Counts{Creates: 7, RelationsAdded: 3}) {
		t.Errorf("counts = %+v, want 7 creates and 3 relations", p.Counts)
	}
	if p.Creates[0].Kind != KindEpic || p.Creates[1].Kind != KindEpic {
		t.Errorf("creates = %+v, want the epics first", p.Creates)
	}
	for _, c := range p.Creates[2:] {
		if c.Kind != KindStory || c.ParentKey == "" {
			t.Errorf("story create %+v has no parent", c)
		}
	}
	if len(p.MissingCapabilities) != 0 {
		t.Errorf("missing = %v, want none", p.MissingCapabilities)
	}
	if f.writes != 0 {
		t.Errorf("a preview made %d writes", f.writes)
	}
}

func TestPreviewSecondRunHasNoWrites(t *testing.T) {
	t.Parallel()

	g := firstPlan().build()
	f := newFakeTracker()
	seedMatching(f, g)
	p := preview(t, g, f.tracker())
	if p.Writes() != 0 || p.Counts != (Counts{}) {
		t.Errorf("second preview = %+v, want no writes", p.Counts)
	}
}

func TestPreviewChangedTitleUpdates(t *testing.T) {
	t.Parallel()

	g := firstPlan().build()
	f := newFakeTracker()
	seedMatching(f, g)
	for i := range g.Nodes {
		if g.Nodes[i].PlanKey == "billing/tax" {
			g.Nodes[i].Title = "Apply tax rules"
		}
	}
	p := preview(t, g, f.tracker())
	if len(p.Creates) != 0 || len(p.Updates) != 1 || p.Updates[0].PlanKey != "billing/tax" ||
		!slices.Equal(p.Updates[0].Fields, []string{"title"}) {
		t.Errorf("preview = creates %v updates %+v, want one title update", p.Creates, p.Updates)
	}
}

func TestPreviewRemovedDeliverableIsOrphaned(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	seedMatching(f, firstPlan().build())
	// billing/tax leaves the plan.
	g := newGraph().
		epic("accounts-api", ApprovalApproved, "open", "close", "audit").
		epic("billing", ApprovalApproved, "invoice").
		block("accounts-api", "billing").
		block("accounts-api/close", "billing/invoice").build()

	p := preview(t, g, f.tracker())
	if len(p.Orphans) != 1 || p.Orphans[0].PlanKey != "billing/tax" {
		t.Errorf("orphans = %+v, want billing/tax", p.Orphans)
	}
	if !slices.Equal(p.RelationsRemoved, []RelationOp{{Blocker: "billing/invoice", Blocked: "billing/tax"}}) {
		t.Errorf("relations removed = %+v, want the orphan's relation", p.RelationsRemoved)
	}
	if p.Writes() != 1 {
		t.Errorf("writes = %d, want only the relation removal (no delete)", p.Writes())
	}
}

func TestPreviewSkipsDrafts(t *testing.T) {
	t.Parallel()

	g := newGraph().epic("accounts-api", ApprovalApproved, "open").epic("billing", ApprovalDraft, "invoice").
		block("accounts-api/open", "billing/invoice").build()
	g.UnplannedChanges = []string{"search-index"}
	p := preview(t, g, newFakeTracker().tracker())
	if !slices.Equal(p.SkippedDrafts, []string{"billing", "billing/invoice"}) || !slices.Equal(p.UnplannedChanges, []string{"search-index"}) {
		t.Errorf("skipped %v unplanned %v", p.SkippedDrafts, p.UnplannedChanges)
	}
	for _, c := range p.Creates {
		if c.PlanKey == "billing" || c.PlanKey == "billing/invoice" {
			t.Errorf("draft node %s is created", c.PlanKey)
		}
	}
	if len(p.RelationsAdded) != 0 {
		t.Errorf("relations = %+v, want none to a draft node", p.RelationsAdded)
	}
}

func TestPreviewLeavesOthersRelationsAlone(t *testing.T) {
	t.Parallel()

	g := firstPlan().build()
	f := newFakeTracker()
	seedMatching(f, g)
	// A person links an unkeyed bug to a story.
	f.issues = append(f.issues, &domain.PlanIssue{ID: "bug", Identifier: "T-99", BlockedBy: []string{}})
	f.byID("seed-billing/tax").BlockedBy = append(f.byID("seed-billing/tax").BlockedBy, "bug")
	// billing becomes a draft: its synced relations must stay.
	for i := range g.Nodes {
		if g.Nodes[i].Change == "billing" {
			g.Nodes[i].Approval = ApprovalDraft
		}
	}
	p := preview(t, g, f.tracker())
	if len(p.RelationsRemoved) != 0 || len(p.Orphans) != 0 {
		t.Errorf("removed %+v orphans %+v, want neither", p.RelationsRemoved, p.Orphans)
	}
}

func TestPreviewMissingCapabilities(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	tr := f.tracker()
	tr.Meta.WriteCapabilities = []registry.WriteCapability{registry.CapCreateIssue, registry.CapSetParent}
	p := preview(t, firstPlan().build(), tr)
	if !slices.Equal(p.MissingCapabilities, []registry.WriteCapability{registry.CapLinkDependency}) {
		t.Errorf("missing = %v, want link_dependency", p.MissingCapabilities)
	}

	// A tracker without any write capability can still be previewed.
	none := Tracker{Adapter: f, Meta: registry.TrackerMeta{}}
	p = preview(t, firstPlan().build(), none)
	if len(p.Creates) != 7 || len(p.MissingCapabilities) != 2 {
		t.Errorf("preview without capabilities = %d creates, missing %v", len(p.Creates), p.MissingCapabilities)
	}
}

func TestPreviewTrackerDigest(t *testing.T) {
	t.Parallel()

	g := firstPlan().build()
	f := newFakeTracker()
	seedMatching(f, g)
	before := preview(t, g, f.tracker()).TrackerDigest
	if again := preview(t, g, f.tracker()).TrackerDigest; again != before {
		t.Error("tracker digest is not stable")
	}
	f.byID("seed-billing").Title = "renamed by a person"
	if after := preview(t, g, f.tracker()).TrackerDigest; after == before {
		t.Error("tracker digest did not change with the tracker")
	}
}

func TestPreviewTrackerErrorIsReturned(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	f.listErr = &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "down"}
	if _, err := ComputePreview(context.Background(), firstPlan().build(), f.tracker()); err == nil {
		t.Error("ComputePreview() error = nil, want the tracker error")
	}
}
