package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// mgcRecordingStore adds the plan satisfaction capability to the
// merge-completion store fake.
type mgcRecordingStore struct {
	*mgcStoreFake
	records []persistence.PlanSatisfaction
	err     error
}

func (s *mgcRecordingStore) UpsertPlanSatisfaction(_ context.Context, r persistence.PlanSatisfaction) error {
	if s.err != nil {
		return s.err
	}
	s.records = append(s.records, r)
	return nil
}

func TestReconcileMergeCompletion_RecordsThePlanMerge(t *testing.T) {
	t.Parallel()
	issueID := "MGC-PLAN"
	state := mgcStateWithPending(issueID, 42)
	store := &mgcRecordingStore{mgcStoreFake: newMGCStore()}
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-9"), nil }}

	reconcileMergeCompletion(state, mgcParams(store, scm, tracker), discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(store.records) != 1 {
		t.Fatalf("plan satisfaction records = %d, want 1", len(store.records))
	}
	r := store.records[0]
	if r.IssueID != issueID || r.Kind != persistence.PlanSatisfactionMerged || r.Ref != "sha-9" || r.PR == "" {
		t.Errorf("record = %+v, want merged sha-9 with a pull request", r)
	}
	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
	}
}

// A failed record keeps the entry pending and does not transition, so the
// merge is never forgotten.
func TestReconcileMergeCompletion_FailedPlanRecordRetries(t *testing.T) {
	t.Parallel()
	issueID := "MGC-PLAN-FAIL"
	state := mgcStateWithPending(issueID, 43)
	store := &mgcRecordingStore{mgcStoreFake: newMGCStore(), err: errors.New("disk full")}
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-10"), nil }}

	reconcileMergeCompletion(state, mgcParams(store, scm, tracker), discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 after a failed record", len(tracker.transitionCalls))
	}
	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)]; !ok {
		t.Error("entry dropped, want it kept for a retry")
	}
}

// The record lives in the database, not the workspace: a store reopened
// on the same file still has it.
func TestPlanMerge_SurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := t.TempDir() + "/sortie.db"
	s, err := persistence.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recordPlanMerge(s, "iss-1", "sha-1", &MergeCompletionReactionData{PRNumber: 3, Owner: "o", Repo: "r"}, ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s, err = persistence.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // test
	got, err := s.LoadPlanSatisfaction(ctx, []string{"iss-1"})
	if err != nil || len(got["iss-1"]) != 1 || got["iss-1"][0].PR != "o/r#3" {
		t.Errorf("after reopen = %+v, %v, want the merge with o/r#3", got, err)
	}
}
