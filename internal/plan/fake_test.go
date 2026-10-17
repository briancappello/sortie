package plan

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// fakeTracker is an in-memory tracker that implements every write
// capability. failAt, when positive, fails the write with that 1-based
// sequence number with a transport error.
type fakeTracker struct {
	domain.TrackerAdapter

	mu      sync.Mutex
	issues  []*domain.PlanIssue
	writes  int
	failAt  int
	listErr error
	seq     int
	log     []string
}

func newFakeTracker() *fakeTracker { return &fakeTracker{} }

func (f *fakeTracker) tracker() Tracker {
	return Tracker{Adapter: f, Meta: registry.TrackerMeta{
		WriteCapabilities: registry.AllWriteCapabilities(),
		PlanKeyStorage:    registry.PlanKeyCustomField,
	}}
}

func (f *fakeTracker) write(op string) error {
	f.writes++
	if f.failAt > 0 && f.writes == f.failAt {
		return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "fake: injected failure on " + op}
	}
	f.log = append(f.log, op)
	return nil
}

func (f *fakeTracker) byID(id string) *domain.PlanIssue {
	for _, i := range f.issues {
		if i.ID == id {
			return i
		}
	}
	return nil
}

func (f *fakeTracker) FindIssueByPlanKey(_ context.Context, key string) (domain.PlanIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.issues {
		if i.PlanKey == key {
			return *i, nil
		}
	}
	return domain.PlanIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: key}
}

func (f *fakeTracker) ListPlanIssues(_ context.Context, includeUnkeyed bool) ([]domain.PlanIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []domain.PlanIssue{}
	for _, i := range f.issues {
		if includeUnkeyed || i.PlanKey != "" {
			c := *i
			c.BlockedBy = slices.Clone(i.BlockedBy)
			c.Labels = slices.Clone(i.Labels)
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeTracker) CreateIssue(_ context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.write("create " + in.PlanKey); err != nil {
		return domain.CreatedIssue{}, err
	}
	f.seq++
	id := fmt.Sprintf("id-%d", f.seq)
	f.issues = append(f.issues, &domain.PlanIssue{
		ID: id, Identifier: fmt.Sprintf("T-%d", f.seq), PlanKey: in.PlanKey, Kind: in.Kind,
		Title: in.Title, BodyDigest: BodyDigest(in.Body), State: cmp.Or(in.State, "Backlog"), ParentID: in.ParentID, BlockedBy: []string{},
		Labels: []string{},
	})
	return domain.CreatedIssue{ID: id, Identifier: fmt.Sprintf("T-%d", f.seq)}, nil
}

func (f *fakeTracker) UpdateIssue(_ context.Context, id string, fields domain.IssueFields) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.byID(id)
	if err := f.write("update " + i.PlanKey); err != nil {
		return err
	}
	if fields.Title != nil {
		i.Title = *fields.Title
	}
	if fields.Body != nil {
		i.BodyDigest = BodyDigest(*fields.Body)
	}
	if fields.Kind != nil {
		i.Kind = *fields.Kind
	}
	return nil
}

func (f *fakeTracker) SetParent(_ context.Context, id, parentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.byID(id)
	if err := f.write("parent " + i.PlanKey); err != nil {
		return err
	}
	i.ParentID = parentID
	return nil
}

func (f *fakeTracker) AddDependency(_ context.Context, blockerID, blockedID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.byID(blockedID)
	if err := f.write("link " + f.byID(blockerID).PlanKey + ">" + b.PlanKey); err != nil {
		return err
	}
	if !slices.Contains(b.BlockedBy, blockerID) {
		b.BlockedBy = append(b.BlockedBy, blockerID)
	}
	return nil
}

func (f *fakeTracker) RemoveDependency(_ context.Context, blockerID, blockedID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.byID(blockedID)
	if err := f.write("unlink " + f.byID(blockerID).PlanKey + ">" + b.PlanKey); err != nil {
		return err
	}
	b.BlockedBy = slices.DeleteFunc(b.BlockedBy, func(s string) bool { return s == blockerID })
	return nil
}

// graphBuilder builds plan graphs for tests.
type graphBuilder struct{ g Graph }

func newGraph() *graphBuilder {
	return &graphBuilder{g: Graph{
		SchemaVersion: 1, SourceDigest: "sha256:" + fmt.Sprintf("%064d", 1),
		Nodes: []Node{}, Parents: []ParentEdge{}, Blocks: []BlockEdge{}, UnplannedChanges: []string{},
	}}
}

func (b *graphBuilder) epic(change, approval string, stories ...string) *graphBuilder {
	b.g.Nodes = append(b.g.Nodes, Node{PlanKey: change, Kind: KindEpic, Change: change, Title: change,
		Body: "change " + change, Approval: approval, Covers: []string{}, AcceptanceCriteria: []string{}})
	for _, s := range stories {
		key := change + "/" + s
		dk := "story"
		b.g.Nodes = append(b.g.Nodes, Node{PlanKey: key, Kind: KindStory, Change: change, Title: "Do " + s,
			Body: "body " + s, Approval: approval, DeliverableKind: &dk, Covers: []string{}, AcceptanceCriteria: []string{"ok"}})
		b.g.Parents = append(b.g.Parents, ParentEdge{Child: key, Parent: change})
	}
	return b
}

func (b *graphBuilder) block(blocker, blocked string) *graphBuilder {
	b.g.Blocks = append(b.g.Blocks, BlockEdge{Blocker: blocker, Blocked: blocked})
	return b
}

func (b *graphBuilder) build() Graph { return b.g }

func (f *fakeTracker) TransitionIssue(_ context.Context, id, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.byID(id)
	if err := f.write("transition " + i.PlanKey + ">" + state); err != nil {
		return err
	}
	i.State = state
	return nil
}

func (f *fakeTracker) AddLabel(_ context.Context, id, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.byID(id)
	if err := f.write("label+ " + i.PlanKey + " " + label); err != nil {
		return err
	}
	if !slices.Contains(i.Labels, label) {
		i.Labels = append(i.Labels, label)
	}
	return nil
}

func (f *fakeTracker) RemoveLabel(_ context.Context, id, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.byID(id)
	if err := f.write("label- " + i.PlanKey + " " + label); err != nil {
		return err
	}
	i.Labels = slices.DeleteFunc(i.Labels, func(l string) bool { return l == label })
	return nil
}

func (f *fakeTracker) SetApproval(_ context.Context, id string, a domain.PlanApproval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.byID(id)
	if err := f.write("approve " + i.PlanKey); err != nil {
		return err
	}
	i.Approval = &a
	return nil
}

// planControlled returns the tracker with plan control on.
func (f *fakeTracker) planControlled() Tracker {
	t := f.tracker()
	t.States = func() domain.TrackerStates { return domain.TrackerStates{Release: "Todo", Backlog: "Backlog"} }
	return t
}

// noApprovals is a fake whose kind does not declare approval fields.
func (f *fakeTracker) noApprovals() Tracker {
	t := f.planControlled()
	t.Meta.WriteCapabilities = []registry.WriteCapability{registry.CapCreateIssue, registry.CapUpdateIssue, registry.CapSetParent, registry.CapLinkDependency}
	return t
}
