package plan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func applyFresh(t *testing.T, s *Syncer, g Graph) (Result, error) {
	t.Helper()
	_, id, err := s.Preview(context.Background(), g)
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	return s.Apply(context.Background(), g, id)
}

func TestApplyFirstSyncInDependencyOrder(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	s := NewSyncer(f.tracker())
	g := firstPlan().build()
	r, err := applyFresh(t, s, g)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(r.Created) != 7 || len(r.RelationsAdded) != 3 || r.Failed != nil {
		t.Fatalf("result = %+v", r)
	}
	// Epics are created before stories, and relations come last.
	want := []string{"create accounts-api", "create billing"}
	if !slices.Equal(f.log[:2], want) || !strings.HasPrefix(f.log[len(f.log)-1], "link ") {
		t.Errorf("write order = %v", f.log)
	}
	// Each story is parented to its epic.
	issues, _ := f.ListPlanIssues(context.Background(), false)
	byKey := map[string]domain.PlanIssue{}
	for _, i := range issues {
		byKey[i.PlanKey] = i
	}
	if byKey["billing/tax"].ParentID != byKey["billing"].ID || !slices.Equal(byKey["billing"].BlockedBy, []string{byKey["accounts-api"].ID}) {
		t.Errorf("billing/tax parent %q, billing blocked by %v", byKey["billing/tax"].ParentID, byKey["billing"].BlockedBy)
	}

	// A second sync of the same graph writes nothing.
	writes := f.writes
	p, _, _ := s.Preview(context.Background(), g)
	if p.Writes() != 0 {
		t.Errorf("second preview = %+v, want no writes", p.Counts)
	}
	if _, err := applyFresh(t, s, g); err != nil || f.writes != writes {
		t.Errorf("second apply wrote %d times, err %v", f.writes-writes, err)
	}
}

func TestApplyRefusesMissingCapabilityBeforeWriting(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	tr := f.tracker()
	tr.Meta.WriteCapabilities = []registry.WriteCapability{registry.CapCreateIssue, registry.CapUpdateIssue, registry.CapSetParent}
	_, err := applyFresh(t, NewSyncer(tr), firstPlan().build())
	te, ok := errors.AsType[*domain.TrackerError](err)
	if !ok || te.Kind != domain.ErrTrackerUnsupportedOperation || !strings.Contains(err.Error(), "link_dependency") {
		t.Fatalf("Apply() error = %v, want tracker_unsupported_operation naming link_dependency", err)
	}
	if f.writes != 0 {
		t.Errorf("writes = %d, want none", f.writes)
	}
}

func TestApplyRefusesStalePreview(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newFakeTracker()
	s := NewSyncer(f.tracker())
	g := firstPlan().build()

	_, id, _ := s.Preview(ctx, g)
	changed := g
	changed.SourceDigest = "sha256:" + fmt.Sprintf("%064d", 2)
	if _, err := s.Apply(ctx, changed, id); !isConflict(err) {
		t.Errorf("Apply(changed graph) error = %v, want a conflict", err)
	}

	_, id, _ = s.Preview(ctx, g)
	f.issues = append(f.issues, &domain.PlanIssue{ID: "x", PlanKey: "billing", Title: "made by hand", BlockedBy: []string{}})
	if _, err := s.Apply(ctx, g, id); !isConflict(err) {
		t.Errorf("Apply(changed tracker) error = %v, want a conflict", err)
	}

	if _, err := s.Apply(ctx, g, "no-such-preview"); !isConflict(err) {
		t.Errorf("Apply(unknown preview) error = %v, want a conflict", err)
	}
	if f.writes != 0 {
		t.Errorf("writes = %d, want none", f.writes)
	}
}

func isConflict(err error) bool {
	_, ok := errors.AsType[*ConflictError](err)
	return ok
}

func TestApplyStopsAtFirstFailureAndResumes(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	f.failAt = 4
	s := NewSyncer(f.tracker())
	g := firstPlan().build()

	r, err := applyFresh(t, s, g)
	if err == nil || r.Failed == nil || r.Failed.Op != "create" {
		t.Fatalf("Apply() = %+v, %v; want a failed create", r, err)
	}
	if len(r.Created) != 3 || len(r.RelationsAdded) != 0 {
		t.Errorf("progress = %d created, %d relations; want 3 and 0", len(r.Created), len(r.RelationsAdded))
	}

	f.failAt = 0
	r, err = applyFresh(t, s, g)
	if err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if len(r.Created) != 4 || len(r.RelationsAdded) != 3 {
		t.Errorf("resume = %d created, %d relations; want the remaining 4 and 3", len(r.Created), len(r.RelationsAdded))
	}
	keys := map[string]int{}
	for _, i := range f.issues {
		keys[i.PlanKey]++
	}
	for k, n := range keys {
		if n != 1 {
			t.Errorf("%s has %d issues, want 1", k, n)
		}
	}
	if len(keys) != 7 {
		t.Errorf("issues = %d, want 7", len(keys))
	}
}

func TestApplyUpdatesParentsAndRemovesRelations(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	s := NewSyncer(f.tracker())
	if _, err := applyFresh(t, s, firstPlan().build()); err != nil {
		t.Fatal(err)
	}
	// A person moves billing/tax to the top level; the plan renames it and
	// drops its relation.
	tax, _ := f.FindIssueByPlanKey(context.Background(), "billing/tax")
	f.byID(tax.ID).ParentID = ""
	g := newGraph().
		epic("accounts-api", ApprovalApproved, "open", "close", "audit").
		epic("billing", ApprovalApproved, "invoice", "tax").
		block("accounts-api", "billing").
		block("accounts-api/close", "billing/invoice").build()
	for i := range g.Nodes {
		if g.Nodes[i].PlanKey == "billing/tax" {
			g.Nodes[i].Title = "Apply tax"
		}
	}
	r, err := applyFresh(t, s, g)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ParentsSet) != 1 || len(r.Updated) != 1 || len(r.RelationsRemoved) != 1 || len(r.Created) != 0 {
		t.Errorf("result = %+v", r)
	}
	tax, _ = f.FindIssueByPlanKey(context.Background(), "billing/tax")
	billing, _ := f.FindIssueByPlanKey(context.Background(), "billing")
	if tax.Title != "Apply tax" || tax.ParentID != billing.ID || len(tax.BlockedBy) != 0 {
		t.Errorf("billing/tax = %+v", tax)
	}
}

// blockingTracker holds its first create until release is closed.
type blockingTracker struct {
	*fakeTracker
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingTracker) CreateIssue(ctx context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.fakeTracker.CreateIssue(ctx, in)
}

func TestApplyIsExclusive(t *testing.T) {
	t.Parallel()

	b := &blockingTracker{fakeTracker: newFakeTracker(), started: make(chan struct{}), release: make(chan struct{})}
	tr := b.tracker()
	tr.Adapter = b
	s := NewSyncer(tr)
	g := firstPlan().build()

	done := make(chan error, 1)
	go func() {
		_, _, err := s.ApplyNow(context.Background(), g)
		done <- err
	}()
	<-b.started
	if _, err := applyFresh(t, s, g); !errors.Is(err, ErrApplyInProgress) {
		t.Errorf("concurrent Apply() error = %v, want ErrApplyInProgress", err)
	}
	if _, _, err := s.ApplyNow(context.Background(), g); !errors.Is(err, ErrApplyInProgress) {
		t.Errorf("concurrent ApplyNow() error = %v, want ErrApplyInProgress", err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatalf("first apply error = %v", err)
	}
}
