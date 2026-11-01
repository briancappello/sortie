package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

type fakeSatisfaction struct {
	rows  map[string][]persistence.PlanSatisfaction
	err   error
	mu    sync.Mutex
	calls int
}

func (f *fakeSatisfaction) LoadPlanSatisfaction(_ context.Context, ids []string) (map[string][]persistence.PlanSatisfaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string][]persistence.PlanSatisfaction{}
	for _, id := range ids {
		if r, ok := f.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

var planTerminal = stateSet([]string{"Done", "Canceled", "Merged"})

func planTracker() config.TrackerConfig {
	return config.TrackerConfig{
		ReleaseState: "Todo", BacklogState: "Backlog", CancelState: "Canceled", DoneState: "Done",
		ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done", "Canceled", "Merged"},
	}
}

func merged(id string) persistence.PlanSatisfaction {
	return persistence.PlanSatisfaction{IssueID: id, Kind: persistence.PlanSatisfactionMerged, Ref: "sha-" + id}
}

func TestPlanJudge_Scenarios(t *testing.T) {
	t.Parallel()
	store := &fakeSatisfaction{rows: map[string][]persistence.PlanSatisfaction{
		"a3-merged":     {merged("a3-merged")},
		"commit-claim":  {{IssueID: "commit-claim", Kind: persistence.PlanSatisfactionNoChange, Ref: "commit:abc"}},
		"cites-merged":  {{IssueID: "cites-merged", Kind: persistence.PlanSatisfactionNoChange, Ref: "plan:x", Cites: "a3-merged"}},
		"cites-open":    {{IssueID: "cites-open", Kind: persistence.PlanSatisfactionNoChange, Ref: "plan:y", Cites: "open"}},
		"cites-chain":   {{IssueID: "cites-chain", Kind: persistence.PlanSatisfactionNoChange, Ref: "plan:z", Cites: "cites-merged"}},
		"cites-self":    {{IssueID: "cites-self", Kind: persistence.PlanSatisfactionNoChange, Ref: "plan:s", Cites: "cites-self"}},
		"unverified":    {{IssueID: "unverified", Kind: persistence.PlanSatisfactionUnverified, Ref: "no_mirror"}},
		"canceled-also": {merged("canceled-also")},
	}}
	tests := []struct {
		name string
		b    domain.BlockerRef
		want string
	}{
		{"a blocker merged", domain.BlockerRef{ID: "a3-merged", PlanKey: "billing/a3", State: "In Review"}, ""},
		{"a blocker a person moved to Done", domain.BlockerRef{ID: "open", PlanKey: "billing/a3", State: "Done"}, BlockerAwaitingMerge},
		{"a blocker not merged", domain.BlockerRef{ID: "open", PlanKey: "billing/a3", State: "In Progress"}, BlockerNotMerged},
		{"a cancelled blocker", domain.BlockerRef{ID: "open", PlanKey: "billing/a3", State: "Canceled"}, BlockerCanceled},
		{"cancelled beats a merge record", domain.BlockerRef{ID: "canceled-also", PlanKey: "billing/a3", State: "canceled"}, BlockerCanceled},
		{"an unkeyed blocker done", domain.BlockerRef{ID: "HULY-88", State: "Done"}, ""},
		{"an unkeyed blocker cancelled", domain.BlockerRef{ID: "HULY-88", State: "Canceled"}, BlockerCanceled},
		{"an unkeyed blocker in another terminal state", domain.BlockerRef{ID: "HULY-88", State: "Merged"}, BlockerNotDone},
		{"an unreadable state", domain.BlockerRef{ID: "a3-merged", PlanKey: "billing/a3"}, BlockerUnknown},
		{"a confirmed commit claim", domain.BlockerRef{ID: "commit-claim", PlanKey: "k", State: "Done"}, ""},
		{"a claim citing a merged story", domain.BlockerRef{ID: "cites-merged", PlanKey: "k", State: "Done"}, ""},
		{"a claim citing an open story", domain.BlockerRef{ID: "cites-open", PlanKey: "k", State: "Done"}, BlockerAwaitingMerge},
		{"citations resolve one level only", domain.BlockerRef{ID: "cites-chain", PlanKey: "k", State: "Done"}, BlockerAwaitingMerge},
		{"a claim citing itself", domain.BlockerRef{ID: "cites-self", PlanKey: "k", State: "Done"}, BlockerAwaitingMerge},
		{"an unverified claim", domain.BlockerRef{ID: "unverified", PlanKey: "k", State: "Done"}, BlockerUnverifiedNoChange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := NewPlanJudge(context.Background(), store, "Canceled", "Done", discardLogger())
			if got := j.Unsatisfied(tt.b, planTerminal); got != tt.want {
				t.Errorf("Unsatisfied(%+v) = %q, want %q", tt.b, got, tt.want)
			}
		})
	}
}

// An adapter that stores no plan keys leaves PlanKey empty, so every
// blocker follows the done-state rule.
func TestPlanJudge_AdapterWithoutPlanKeys(t *testing.T) {
	t.Parallel()
	j := NewPlanJudge(context.Background(), &fakeSatisfaction{}, "Canceled", "Done", discardLogger())
	if got := j.Unsatisfied(domain.BlockerRef{ID: "x", State: "Done"}, planTerminal); got != "" {
		t.Errorf("Done unkeyed = %q, want satisfied", got)
	}
}

// A failed read satisfies no plan-keyed blocker.
func TestPlanJudge_ReadFailureHolds(t *testing.T) {
	t.Parallel()
	store := &fakeSatisfaction{rows: map[string][]persistence.PlanSatisfaction{"a": {merged("a")}}, err: errors.New("locked")}
	j := NewPlanJudge(context.Background(), store, "Canceled", "Done", discardLogger())
	j.Prime([]string{"a"})
	if got := j.Unsatisfied(domain.BlockerRef{ID: "a", PlanKey: "k", State: "Done"}, planTerminal); got == "" {
		t.Error("satisfied after a failed read, want held")
	}
}

// Without plan control the old rule holds: any terminal blocker satisfies.
func TestPlanJudgeFor_OffKeepsTheTerminalRule(t *testing.T) {
	t.Parallel()
	if j := PlanJudgeFor(context.Background(), config.TrackerConfig{}, &fakeSatisfaction{}, nil, nil, discardLogger()); j != nil {
		t.Fatalf("PlanJudgeFor() = %T, want nil with plan control off", j)
	}
	st := &State{}
	issue := domain.Issue{BlockedBy: []domain.BlockerRef{{ID: "x", PlanKey: "k", State: "Canceled"}}}
	if isBlockedFor(judgeOf(st), issue, planTerminal) {
		t.Error("a Canceled blocker holds with plan control off, want satisfied as before")
	}
}

// One tick loads satisfaction once for all blockers, plus once for the
// stories their claims cite, and never again while judging.
func TestPlanJudgeFor_OneLoadPerTick(t *testing.T) {
	t.Parallel()
	store := &fakeSatisfaction{rows: map[string][]persistence.PlanSatisfaction{
		"b1": {{IssueID: "b1", Kind: persistence.PlanSatisfactionNoChange, Cites: "c1"}},
	}}
	candidates := make([]domain.Issue, 50)
	for i := range candidates {
		candidates[i] = domain.Issue{BlockedBy: []domain.BlockerRef{{ID: "b1", PlanKey: "k", State: "Done"}, {ID: "b2", PlanKey: "k2", State: "Done"}}}
	}
	j := PlanJudgeFor(context.Background(), planTracker(), store, candidates, nil, discardLogger())
	for _, c := range candidates {
		_ = isBlockedFor(j, c, planTerminal)
	}
	if store.calls != 2 {
		t.Errorf("store loads = %d, want 2 (blockers, then cited stories)", store.calls)
	}
}

// EvaluateCandidate, ShouldDispatch, and the retry lane all read the
// state's judge.
func TestPlanJudge_SharedByDispatchPaths(t *testing.T) {
	t.Parallel()
	j := NewPlanJudge(context.Background(), &fakeSatisfaction{}, "Canceled", "Done", discardLogger())
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	st.BlockerJudge = j
	issue := domain.Issue{ID: "i1", Identifier: "P-1", Title: "t", State: "Todo",
		BlockedBy: []domain.BlockerRef{{ID: "a3", PlanKey: "billing/a3", State: "Done"}}}
	active := stateSet([]string{"Todo"})

	if ShouldDispatch(issue, st, []string{"Todo"}, []string{"Done", "Canceled"}) {
		t.Error("ShouldDispatch dispatches past an unmerged plan blocker")
	}
	if ShouldDispatchWithSets(issue, st, active, planTerminal) {
		t.Error("ShouldDispatchWithSets dispatches past an unmerged plan blocker")
	}
	d := EvaluateCandidate(context.Background(), issue, st, active, planTerminal, nil, &TickResolution{})
	if d.Dispatch || d.Reason != SkipBlockedBy {
		t.Errorf("EvaluateCandidate = %+v, want held by blocker", d)
	}
	if b, why := firstUnsatisfiedBlocker(judgeOf(st), issue.BlockedBy, planTerminal); b.ID != "a3" || why != BlockerAwaitingMerge {
		t.Errorf("hold log blocker = %+v %q, want a3 awaiting_merge_record", b, why)
	}
}

// The retry lane holds a plan story whose blocker a person moved to Done
// without a merge, exactly as dispatch does.
func TestPlanJudge_RetryLaneUsesTheJudge(t *testing.T) {
	t.Parallel()
	const id = "PLAN-RETRY"
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.BlockerJudge = NewPlanJudge(context.Background(), &fakeSatisfaction{}, "Canceled", "Done", discardLogger())
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{IssueID: id, Identifier: id, Attempt: 1}

	issue := candidateIssue(id, id, "To Do")
	issue.BlockedBy = []domain.BlockerRef{{ID: "A3", Identifier: "A3", PlanKey: "billing/a3", State: "Done"}}
	params := defaultRetryParams(t, &mockRetryStore{}, &mockRetryTracker{fetchedIssue: issue})

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Error("retry dispatched past an unmerged plan blocker")
	}
	if _, claimed := state.Claimed[id]; claimed {
		t.Error("claim kept, want released as for any blocked retry")
	}
}
