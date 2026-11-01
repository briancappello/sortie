package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/plan"
	"github.com/sortie-ai/sortie/internal/registry"
)

// loopTracker is one in-memory tracker that both plan sync and dispatch
// use, as Huly does: plan writes on one side, candidates with blockers on
// the other.
type loopTracker struct {
	domain.TrackerAdapter
	mu     sync.Mutex
	issues []*domain.PlanIssue
	seq    int
}

func (l *loopTracker) byID(id string) *domain.PlanIssue {
	for _, i := range l.issues {
		if i.ID == id {
			return i
		}
	}
	return nil
}

func (l *loopTracker) byKey(key string) *domain.PlanIssue {
	for _, i := range l.issues {
		if i.PlanKey == key {
			return i
		}
	}
	return nil
}

func (l *loopTracker) FindIssueByPlanKey(_ context.Context, key string) (domain.PlanIssue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i := l.byKey(key); i != nil {
		return *i, nil
	}
	return domain.PlanIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound}
}

func (l *loopTracker) ListPlanIssues(context.Context, bool) ([]domain.PlanIssue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []domain.PlanIssue{}
	for _, i := range l.issues {
		c := *i
		c.BlockedBy, c.Labels = slices.Clone(i.BlockedBy), slices.Clone(i.Labels)
		out = append(out, c)
	}
	return out, nil
}

func (l *loopTracker) CreateIssue(_ context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	id, ident := fmt.Sprintf("id-%d", l.seq), fmt.Sprintf("PCE-%d", l.seq)
	l.issues = append(l.issues, &domain.PlanIssue{ID: id, Identifier: ident, PlanKey: in.PlanKey, Kind: in.Kind,
		Title: in.Title, BodyDigest: plan.BodyDigest(in.Body), State: in.State, ParentID: in.ParentID, BlockedBy: []string{}, Labels: []string{}})
	return domain.CreatedIssue{ID: id, Identifier: ident}, nil
}

func (l *loopTracker) UpdateIssue(context.Context, string, domain.IssueFields) error { return nil }

func (l *loopTracker) SetParent(_ context.Context, id, parent string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID(id).ParentID = parent
	return nil
}

func (l *loopTracker) AddDependency(_ context.Context, blocker, blocked string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.byID(blocked)
	b.BlockedBy = append(b.BlockedBy, blocker)
	return nil
}

func (l *loopTracker) RemoveDependency(context.Context, string, string) error { return nil }

func (l *loopTracker) SetApproval(_ context.Context, id string, a domain.PlanApproval) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID(id).Approval = &a
	return nil
}

func (l *loopTracker) TransitionIssue(_ context.Context, id, state string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID(id).State = state
	return nil
}

func (l *loopTracker) AddLabel(_ context.Context, id, label string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.byID(id)
	i.Labels = append(i.Labels, label)
	return nil
}

func (l *loopTracker) RemoveLabel(context.Context, string, string) error { return nil }

// candidates are the stories in an active state, with their blockers as
// Huly reports them: id, state, and plan key.
func (l *loopTracker) candidates(active ...string) []domain.Issue {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.Issue
	for _, i := range l.issues {
		if i.Kind != domain.IssueKindStory || !slices.Contains(active, i.State) {
			continue
		}
		is := domain.Issue{ID: i.ID, Identifier: i.Identifier, Title: i.Title, State: i.State, Labels: slices.Clone(i.Labels)}
		for _, b := range i.BlockedBy {
			bi := l.byID(b)
			is.BlockedBy = append(is.BlockedBy, domain.BlockerRef{ID: bi.ID, Identifier: bi.Identifier, State: bi.State, PlanKey: bi.PlanKey})
		}
		out = append(out, is)
	}
	return out
}

func keyOf(l *loopTracker, id string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byID(id).PlanKey
}

// TestPlanControlLoop walks the plan-control loop end to end through the
// real sync, store, judge, dispatch gates, pull-back, and cancel.
func TestPlanControlLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := persistence.Open(ctx, t.TempDir()+"/sortie.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck // test
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	tracker := &loopTracker{}
	tcfg := config.TrackerConfig{
		ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done", "Canceled"},
		HandoffState: "In Review", ReleaseState: "Todo", BacklogState: "Backlog", CancelState: "Canceled", DoneState: "Done",
	}
	syncer := plan.NewSyncer(plan.Tracker{
		Adapter: tracker,
		Meta:    registry.TrackerMeta{WriteCapabilities: registry.AllWriteCapabilities(), PlanKeyStorage: registry.PlanKeyCustomField},
		States:  tcfg.States,
	})

	// 1. Sync a version 2 graph: a3 blocks b1; a3, b1, and c1 are released.
	yes, dk := true, "story"
	story := func(id string) plan.Node {
		return plan.Node{PlanKey: "billing/" + id, Kind: plan.KindStory, Change: "billing", Title: "Do " + id, Body: id,
			Approval: plan.ApprovalApproved, DeliverableKind: &dk, Covers: []string{}, AcceptanceCriteria: []string{"ok"},
			Release: &yes, Review: plan.ReviewCriteria, ApprovedBy: &plan.Approver{Name: "Ana", ID: "iss|ana"}, ApprovedAt: "2026-10-17T09:00:00Z"}
	}
	g := plan.Graph{SchemaVersion: 2, SourceDigest: "sha256:" + strings.Repeat("0", 64),
		Nodes: []plan.Node{
			{PlanKey: "billing", Kind: plan.KindEpic, Change: "billing", Title: "billing", Body: "b", Approval: plan.ApprovalApproved, Covers: []string{}, AcceptanceCriteria: []string{}},
			story("a3"), story("b1"), story("c1"),
		},
		Parents:          []plan.ParentEdge{{Child: "billing/a3", Parent: "billing"}, {Child: "billing/b1", Parent: "billing"}, {Child: "billing/c1", Parent: "billing"}},
		Blocks:           []plan.BlockEdge{{Blocker: "billing/a3", Blocked: "billing/b1"}},
		UnplannedChanges: []string{},
	}
	p, id, err := syncer.Preview(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("step 1 preview: %+v", p.Counts)
	result, err := syncer.Apply(ctx, g, id)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Released) != 3 {
		t.Fatalf("step 1: released %+v, want a3, b1, c1", result.Released)
	}
	for _, key := range []string{"billing/a3", "billing/b1", "billing/c1"} {
		i, _ := tracker.FindIssueByPlanKey(ctx, key)
		if i.State != "Todo" || !slices.Contains(i.Labels, "sortie-review:criteria") || i.Approval == nil {
			t.Fatalf("step 1: %s = %+v, want Todo, the criteria label, and its approver", key, i)
		}
	}
	t.Log("step 1: synced; a3, b1, c1 released to Todo with review labels and approvers")

	st := NewState(1000, 4, 0, nil, AgentTotals{})
	active, terminal := stateSet(tcfg.ActiveStates), stateSet(tcfg.TerminalStates)
	dispatchable := func() []string {
		cands := tracker.candidates("Todo", "In Progress")
		st.BlockerJudge = PlanJudgeFor(ctx, tcfg, store, cands, nil, discardLogger())
		var keys []string
		for _, c := range cands {
			if EvaluateCandidate(ctx, c, st, active, terminal, nil, &TickResolution{}).Dispatch {
				keys = append(keys, keyOf(tracker, c.ID))
			}
		}
		slices.Sort(keys)
		return keys
	}

	// 2. a3 and c1 can start; b1 waits on a3.
	if got := dispatchable(); !slices.Equal(got, []string{"billing/a3", "billing/c1"}) {
		t.Fatalf("step 2: dispatchable %v, want a3 and c1", got)
	}
	t.Log("step 2: a3 and c1 dispatchable, b1 held by a3 (not_merged)")

	// 3. Pull back c1 before a poll dispatches it, while a stale read still
	// shows it in Todo.
	stale := tracker.candidates("Todo")
	c1, _ := tracker.FindIssueByPlanKey(ctx, "billing/c1")
	params := planControlParams{Release: "Todo", Backlog: "Backlog", Cancel: "Canceled", Terminal: tcfg.TerminalStates,
		Tracker: tracker, Store: store, Metrics: &domain.NoopMetrics{}, Log: discardLogger()}
	if r := applyPullBack(ctx, st, planTarget{IssueID: c1.ID, Identifier: c1.Identifier, State: c1.State}, params); r.Outcome != ControlDone {
		t.Fatalf("step 3: pull-back = %+v", r)
	}
	for _, c := range stale {
		if c.ID == c1.ID && EvaluateCandidate(ctx, c, st, active, terminal, nil, &TickResolution{}).Dispatch {
			t.Fatal("step 3: c1 dispatched from a stale read after its pull-back")
		}
	}
	if c1, _ = tracker.FindIssueByPlanKey(ctx, "billing/c1"); c1.State != "Backlog" {
		t.Fatalf("step 3: c1 in %s, want Backlog", c1.State)
	}
	t.Log("step 3: c1 pulled back to Backlog; a stale Todo read cannot dispatch it")

	// Dispatch a3: its claim makes a pull-back lose.
	a3, _ := tracker.FindIssueByPlanKey(ctx, "billing/a3")
	st.Claimed[a3.ID] = struct{}{}
	if r := applyPullBack(ctx, st, planTarget{IssueID: a3.ID, Identifier: a3.Identifier, State: "Todo"}, params); r.Outcome != ControlAlreadyClaimed {
		t.Fatalf("step 3: pull-back of a claimed a3 = %+v, want already_claimed", r)
	}
	t.Log("step 3: pull-back of the dispatched a3 refused with already_claimed")

	// 4. a3's pull request merges: merge completion latches the commit.
	delete(st.Claimed, a3.ID)
	if err := recordPlanMerge(store, a3.ID, strings.Repeat("c", 40), &MergeCompletionReactionData{PRNumber: 7, Owner: "acme", Repo: "api"}, ctx); err != nil {
		t.Fatal(err)
	}
	_ = tracker.TransitionIssue(ctx, a3.ID, "Done")
	if got := dispatchable(); !slices.Equal(got, []string{"billing/b1"}) {
		t.Fatalf("step 4: dispatchable %v, want b1 now that a3 merged", got)
	}
	t.Log("step 4: a3 merged and recorded; b1 dispatchable")

	// 5. Cancel b1 while it runs: the worker stops and b1 is Canceled; a
	// story blocked by it would wait with the reason canceled.
	b1, _ := tracker.FindIssueByPlanKey(ctx, "billing/b1")
	stopped := false
	st.Running[b1.ID] = &RunningEntry{Identifier: b1.Identifier, CancelFunc: func() { stopped = true }}
	st.Claimed[b1.ID] = struct{}{}
	if r := applyCancel(ctx, st, planTarget{IssueID: b1.ID, Identifier: b1.Identifier, State: "In Progress"}, params); r.Outcome != ControlDone || !stopped {
		t.Fatalf("step 5: cancel = %+v, worker stopped %v", r, stopped)
	}
	if b1, _ = tracker.FindIssueByPlanKey(ctx, "billing/b1"); b1.State != "Canceled" {
		t.Fatalf("step 5: b1 in %s, want Canceled", b1.State)
	}
	j := NewPlanJudge(ctx, store, "Canceled", "Done", discardLogger())
	if why := j.Unsatisfied(domain.BlockerRef{ID: b1.ID, PlanKey: b1.PlanKey, State: b1.State}, terminal); why != BlockerCanceled {
		t.Fatalf("step 5: a dependent of b1 waits with %q, want canceled", why)
	}
	t.Log("step 5: b1 cancelled while running: worker stopped, issue Canceled, dependents held (canceled)")
}
