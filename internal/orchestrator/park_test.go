package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// The release rule: a state change unparks, a confirmed label removed
// unparks, an unconfirmed label absent stays parked, and a label observed
// while unconfirmed sets and persists the confirmation flag.

func TestObserveParkedStateReleaseRule(t *testing.T) {
	t.Parallel()

	t.Run("state change unparks with trigger state_changed", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}
		state.Parked["ISS-1"] = entry

		stillParked := observeParkedState(context.Background(), state, store, discardLogger(), entry, "ISS-1", "Done")

		if stillParked {
			t.Error("observeParkedState(...) = true, want false after a state change")
		}
		if _, ok := state.Parked["ISS-1"]; ok {
			t.Error("issue remains in state.Parked after an observed state change")
		}
		if len(store.deletedParkedIDs) != 1 || store.deletedParkedIDs[0] != "ISS-1" {
			t.Errorf("DeleteParkedIssue calls = %v, want [ISS-1]", store.deletedParkedIDs)
		}
	})

	t.Run("empty parked_state is backfilled, not released", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-2", Reason: parkReasonAgentBlocked}
		state.Parked["ISS-2"] = entry

		stillParked := observeParkedState(context.Background(), state, store, discardLogger(), entry, "ISS-2", "In Progress")

		if !stillParked {
			t.Error("observeParkedState(...) = false, want true: an empty parked_state backfills rather than releases")
		}
		if entry.ParkedState != "In Progress" {
			t.Errorf("ParkedState after backfill = %q, want %q", entry.ParkedState, "In Progress")
		}
		if _, ok := state.Parked["ISS-2"]; !ok {
			t.Error("issue was released by the backfill observation")
		}
		if len(store.deletedParkedIDs) != 0 {
			t.Errorf("DeleteParkedIssue calls = %v, want none", store.deletedParkedIDs)
		}
	})
}

func TestObserveParkedLabelsReleaseRule(t *testing.T) {
	t.Parallel()

	t.Run("label observed while unconfirmed sets and persists LabelApplied", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-3", Reason: parkReasonAgentBlocked, Label: "needs-human"}
		state.Parked["ISS-3"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-3", []string{"Needs-Human"})

		if !entry.LabelApplied {
			t.Error("LabelApplied = false, want true after observing the parking label")
		}
		if len(store.labelAppliedIDs) != 1 || store.labelAppliedIDs[0] != "ISS-3" {
			t.Errorf("MarkParkedIssueLabelApplied calls = %v, want [ISS-3]", store.labelAppliedIDs)
		}
		if _, ok := state.Parked["ISS-3"]; !ok {
			t.Error("label confirmation released the park, want it to stay")
		}
	})

	t.Run("confirmed label observed absent unparks with trigger label_removed", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-4", Reason: parkReasonAgentBlocked, Label: "needs-human", LabelApplied: true}
		state.Parked["ISS-4"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-4", nil)

		if _, ok := state.Parked["ISS-4"]; ok {
			t.Error("issue remains in state.Parked after a confirmed label was observed removed")
		}
		if len(store.deletedParkedIDs) != 1 || store.deletedParkedIDs[0] != "ISS-4" {
			t.Errorf("DeleteParkedIssue calls = %v, want [ISS-4]", store.deletedParkedIDs)
		}
	})

	t.Run("unconfirmed label observed absent does not unpark", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-5", Reason: parkReasonAgentBlocked, Label: "needs-human"}
		state.Parked["ISS-5"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-5", nil)

		if _, ok := state.Parked["ISS-5"]; !ok {
			t.Error("unconfirmed label absence released the park, want it held")
		}
		if len(store.deletedParkedIDs) != 0 {
			t.Errorf("DeleteParkedIssue calls = %v, want none", store.deletedParkedIDs)
		}
	})
}

// A failed label write must not cost the park: it stays in place with the
// label unconfirmed, and a later release pass that still observes the
// label absent leaves it exactly where a failed write left it.

func TestParkIssueFailedLabelWriteLeavesLabelUnconfirmed(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-LABEL-FAIL"
	store := &stubStore{}
	tracker := newRecordingHandoffTracker()
	tracker.addLabelErr = errors.New("label service unavailable")
	state := NewState(1000, 1, 0, nil, AgentTotals{})
	var logs bytes.Buffer

	parkIssue(state, parkIssueParams{
		IssueID:        issueID,
		Identifier:     "PROJ-LABEL-FAIL",
		ObservedState:  "In Progress",
		Reason:         parkReasonAgentBlocked,
		Label:          "needs-human",
		Store:          store,
		TrackerAdapter: tracker,
		Metrics:        &domain.NoopMetrics{},
		Logger:         debugLogger(t, &logs),
		Ctx:            context.Background(),
	})
	state.TrackerOpsWg.Wait()

	entry := state.Parked[issueID]
	if entry == nil {
		t.Fatal("issue not parked")
	}
	if entry.LabelApplied {
		t.Error("LabelApplied = true after a failed label write, want false")
	}
	if !strings.Contains(logs.String(), "park label write failed") {
		t.Errorf("missing label failure log\nlogs: %s", logs.String())
	}

	observeParkedLabels(context.Background(), state, store, discardLogger(), entry, issueID, nil)

	if _, ok := state.Parked[issueID]; !ok {
		t.Error("release pass unparked an issue whose label write failed, want it held")
	}
	if entry.LabelApplied {
		t.Error("LabelApplied became true after the release pass, want it to stay false")
	}
}

// A successful AddLabel write is not, by itself, confirmation: only a
// later observation of the label on a tracker read sets LabelApplied. A
// tracker whose AddLabel reports success without the label ever becoming
// visible on a read must not release the park it never confirmed.

func TestParkIssueSuccessfulLabelWriteDoesNotConfirmWithoutObservation(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-LABEL-UNCONFIRMED"
	store := &stubStore{}
	tracker := newRecordingHandoffTracker() // AddLabel returns nil by default.

	state := NewState(1000, 1, 0, nil, AgentTotals{})

	parkIssue(state, parkIssueParams{
		IssueID:        issueID,
		Identifier:     "PROJ-UNCONFIRMED",
		ObservedState:  "In Progress",
		Reason:         parkReasonAgentBlocked,
		Label:          "needs-human",
		Store:          store,
		TrackerAdapter: tracker,
		Metrics:        &domain.NoopMetrics{},
		Logger:         discardLogger(),
		Ctx:            context.Background(),
	})
	state.TrackerOpsWg.Wait()

	if calls := tracker.labels(); len(calls) != 1 {
		t.Fatalf("AddLabel calls = %+v, want exactly one successful write", calls)
	}

	entry := state.Parked[issueID]
	if entry == nil {
		t.Fatal("issue not parked")
	}
	if entry.LabelApplied {
		t.Error("LabelApplied = true immediately after a successful write, want false: confirmation comes from observation, not the write result")
	}
	if len(store.labelAppliedIDs) != 0 {
		t.Errorf("MarkParkedIssueLabelApplied calls = %v, want none before any observation", store.labelAppliedIDs)
	}

	observeParkedLabels(context.Background(), state, store, discardLogger(), entry, issueID, nil)

	if entry.LabelApplied {
		t.Error("LabelApplied became true after a read carrying no label")
	}
	if _, ok := state.Parked[issueID]; !ok {
		t.Error("issue was unparked despite label_applied never having been confirmed")
	}
	if len(store.labelAppliedIDs) != 0 {
		t.Errorf("MarkParkedIssueLabelApplied calls = %v, want none", store.labelAppliedIDs)
	}
}

// A park recorded through a store opened on a temporary file survives a
// simulated restart: the in-memory State is discarded and rebuilt purely
// from the durable rows, and the rebuilt state still refuses dispatch.

func TestParkSurvivesRestart(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-RESTART"
	ctx := context.Background()
	dbPath := t.TempDir() + "/test.db"
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	dispatchedAt := baseTime.Add(-time.Hour)
	state := exitStateWithIssue(t, issueID, "In Progress")
	state.StageChains, state.HopCeiling = [][]string{{"triage", "implement"}}, 4
	running := state.Running[issueID]
	running.Issue.Title = "Restart title"
	running.Issue.URL = "https://tracker.test/PROJ-RESTART"
	running.Issue.IssueType = "Story"
	running.Issue.Labels = []string{"feature", "Needs-Human", "stage-implement"}
	running.RuleName = "implement"
	running.StageLineage = StageLineage{PreviousRule: "triage", PreviousOutcome: "succeeded", ChainID: "chain-1", DispatchedAt: dispatchedAt}
	running.AgentKind, running.ConfiguredModel = "claude-code", "sonnet"
	params := HandleWorkerExitParams{
		Store:               store,
		MaxRetryBackoffMS:   300_000,
		OnRetryFire:         noopRetryFire,
		NowFunc:             func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:              discardLogger(),
		HandoffParkingLabel: "needs-human",
	}

	HandleWorkerExit(state, WorkerResult{
		IssueID:        issueID,
		Identifier:     "PROJ-RESTART",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "blocked",
	}, params)
	state.TrackerOpsWg.Wait()

	if _, ok := state.Parked[issueID]; !ok {
		t.Fatal("issue not parked before the simulated restart")
	}
	state.Parked[issueID].LabelApplied = true
	if err := store.MarkParkedIssueLabelApplied(ctx, issueID); err != nil {
		t.Fatalf("MarkParkedIssueLabelApplied: %v", err)
	}

	rows, err := store.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}

	fresh := NewState(1000, 4, 0, nil, AgentTotals{})
	PopulateParked(fresh, rows, discardLogger())

	if _, ok := fresh.Parked[issueID]; !ok {
		t.Fatal("PopulateParked did not restore the park after the restart")
	}
	fresh.StageChains, fresh.HopCeiling = state.StageChains, state.HopCeiling

	want := SnapshotParkedEntry{
		IssueID: issueID, Identifier: "PROJ-RESTART", IssueTitle: "Restart title", IssueURL: "https://tracker.test/PROJ-RESTART",
		IssueType: "Story", IssueLabels: []string{"feature", "stage-implement"}, DispatchedAt: dispatchedAt,
		Reason: parkReasonAgentBlocked, ParkedState: "In Progress", Label: "needs-human", LabelApplied: true,
		RuleName: "implement", AgentKind: "claude-code", ConfiguredModel: "sonnet", ChainID: "chain-1",
		StagePosition: &SnapshotStagePosition{Chain: []string{"triage", "implement"}, Current: "implement", HopCeiling: 4},
	}
	parkedAt := state.Parked[issueID].ParkedAt
	for name, snapshot := range map[string]struct {
		state    *State
		parkedAt time.Time
	}{
		"before the restart": {state, parkedAt},
		"after the restart":  {fresh, parkedAt.UTC().Truncate(time.Second)},
	} {
		want.ParkedAt = snapshot.parkedAt
		if got := RuntimeSnapshot(snapshot.state, baseTime).Parked; len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("park snapshot %s = %+v, want %+v", name, got, want)
		}
	}

	candidate := domain.Issue{ID: issueID, Identifier: "PROJ-RESTART", Title: "T", State: "In Progress"}
	activeSet := stateSet([]string{"In Progress"})
	terminalSet := stateSet([]string{"Done"})

	if ShouldDispatchWithSets(candidate, fresh, activeSet, terminalSet) {
		t.Error("ShouldDispatchWithSets dispatched an issue whose park was restored across a restart")
	}
}

func newParkedOrchestrator(state *State, tracker domain.TrackerAdapter) *Orchestrator {
	return NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
		Store:           &stubStore{},
	})
}

func TestRefreshParkedIssuesNonCandidateStateRead(t *testing.T) {
	t.Parallel()

	const parkedID, candidateID = "MISS-1", "CAND-1"
	candidates := []domain.Issue{{ID: candidateID, Identifier: "PROJ-CAND-1", Title: "T", State: "To Do"}}
	confirmed := func() *ParkedEntry {
		return &ParkedEntry{Identifier: "PROJ-MISS-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress", Label: "needs-human", LabelApplied: true}
	}

	tests := []struct {
		name       string
		reads      map[string]domain.IssueStateAndLabels
		err        error
		wantParked bool
	}{
		{
			name:  "state differs from parked_state unparks",
			reads: map[string]domain.IssueStateAndLabels{parkedID: {State: "Done", Labels: []string{"needs-human"}}},
		},
		{
			name:       "same state and label leaves the park",
			reads:      map[string]domain.IssueStateAndLabels{parkedID: {State: "In Progress", Labels: []string{"needs-human"}}},
			wantParked: true,
		},
		{
			name:       "issue omitted from the returned map leaves the park",
			reads:      map[string]domain.IssueStateAndLabels{},
			wantParked: true,
		},
		{
			name:       "read error leaves the park in place",
			err:        errors.New("tracker unavailable"),
			wantParked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(1000, 1, 0, nil, AgentTotals{})
			state.Parked[parkedID] = confirmed()
			var fetchedIDs []string
			tracker := &mockTrackerAdapter{
				fetchStatesAndLabelsFn: func(_ context.Context, ids []string) (map[string]domain.IssueStateAndLabels, error) {
					fetchedIDs = slices.Clone(ids)
					return tt.reads, tt.err
				},
			}

			newParkedOrchestrator(state, tracker).refreshParkedIssues(context.Background(), candidates, nil)

			if !slices.Equal(fetchedIDs, []string{parkedID}) {
				t.Errorf("FetchIssueStatesAndLabelsByIDs called with %v, want [%s] only", fetchedIDs, parkedID)
			}
			if _, parked := state.Parked[parkedID]; parked != tt.wantParked {
				t.Errorf("parked after refreshParkedIssues = %v, want %v", parked, tt.wantParked)
			}
		})
	}

	t.Run("parking label is observed on a read of an issue the candidates omit", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-MISS-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress", Label: "needs-human"}
		state.Parked[parkedID] = entry
		var labels []string
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(context.Context, []string) (map[string]string, error) {
				return map[string]string{parkedID: "In Progress"}, nil
			},
			fetchStatesAndLabelsFn: func(context.Context, []string) (map[string]domain.IssueStateAndLabels, error) {
				return map[string]domain.IssueStateAndLabels{parkedID: {State: "In Progress", Labels: labels}}, nil
			},
		}
		orchestrator := newParkedOrchestrator(state, tracker)

		labels = []string{"needs-human"}
		orchestrator.refreshParkedIssues(context.Background(), candidates, nil)

		if state.Parked[parkedID] != entry || !entry.LabelApplied {
			t.Fatalf("after the read carrying the label: parked = %v, LabelApplied = %v, want true, true", state.Parked[parkedID] == entry, entry.LabelApplied)
		}

		labels = nil
		orchestrator.refreshParkedIssues(context.Background(), candidates, nil)

		if _, parked := state.Parked[parkedID]; parked {
			t.Error("park remains after a read of the confirmed issue without its label")
		}
	})
}

func sameStoredIdentity(a, b IssueIdentity) bool {
	return a.Title == b.Title && a.URL == b.URL && a.IssueType == b.IssueType && slices.Equal(a.Labels, b.Labels)
}

func TestRefreshParkedIssuesIdentityBackfill(t *testing.T) {
	t.Parallel()

	const id = "BACKFILL-1"
	held := IssueIdentity{Title: "held title", URL: "https://tracker.test/held"}
	read := domain.Issue{ID: id, Title: "read title", URL: "https://tracker.test/read"}
	readIdentity := IssueIdentity{Title: read.Title, URL: read.URL}

	tests := []struct {
		name         string
		held         IssueIdentity
		parkLabel    string
		candidates   []domain.Issue
		observed     string
		readErr      error
		wantIdentity IssueIdentity
		wantParked   bool
		wantReads    []string
		wantRows     []IssueIdentity
	}{
		{
			name:         "candidate listing the issue supplies the identity",
			candidates:   []domain.Issue{{ID: id, Title: read.Title, URL: read.URL, State: "In Progress"}},
			observed:     "In Progress",
			wantIdentity: readIdentity,
			wantParked:   true,
			wantRows:     []IssueIdentity{readIdentity},
		},
		{
			name:         "partial read takes the candidate's type and labels without the parking label",
			held:         held,
			parkLabel:    "Needs-Human",
			candidates:   []domain.Issue{{ID: id, Title: read.Title, URL: read.URL, State: "In Progress", IssueType: "Story", Labels: []string{"feature", "needs-human"}}},
			observed:     "In Progress",
			wantIdentity: IssueIdentity{Title: held.Title, URL: held.URL, IssueType: "Story", Labels: []string{"feature"}},
			wantParked:   true,
			wantRows:     []IssueIdentity{{Title: held.Title, URL: held.URL, IssueType: "Story", Labels: []string{"feature"}}},
		},
		{
			name:         "issue the candidates omit takes one single-issue read",
			observed:     "In Progress",
			wantIdentity: readIdentity,
			wantParked:   true,
			wantReads:    []string{id},
			wantRows:     []IssueIdentity{readIdentity},
		},
		{
			name:         "held identity is kept and not read again",
			held:         held,
			observed:     "In Progress",
			wantIdentity: held,
			wantParked:   true,
		},
		{
			name:     "park released by the state rule is neither read nor written",
			observed: "Done",
		},
		{
			name:       "failed single-issue read leaves the park as it was",
			observed:   "In Progress",
			readErr:    errors.New("tracker unavailable"),
			wantParked: true,
			wantReads:  []string{id},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(1000, 1, 0, nil, AgentTotals{})
			state.Parked[id] = &ParkedEntry{
				Identifier:    "PROJ-BACKFILL-1",
				Reason:        parkReasonAgentBlocked,
				ParkedState:   "In Progress",
				Label:         tt.parkLabel,
				IssueIdentity: tt.held,
			}
			tracker := &mockTrackerAdapter{
				fetchStatesFn: func(context.Context, []string) (map[string]string, error) {
					return map[string]string{id: tt.observed}, nil
				},
				fetchIssueByIDFn: func(context.Context, string) (domain.Issue, error) {
					return read, tt.readErr
				},
			}
			store := &stubStore{}
			orchestrator := NewOrchestrator(OrchestratorParams{
				State:           state,
				Logger:          discardLogger(),
				TrackerAdapter:  tracker,
				AgentAdapter:    &mockAgentAdapter{},
				WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
				Store:           store,
			})

			orchestrator.refreshParkedIssues(context.Background(), tt.candidates, nil)

			entry, parked := state.Parked[id]
			if parked != tt.wantParked {
				t.Fatalf("parked after refreshParkedIssues = %v, want %v", parked, tt.wantParked)
			}
			if parked && !sameStoredIdentity(entry.IssueIdentity, tt.wantIdentity) {
				t.Errorf("parked IssueIdentity = %+v, want %+v", entry.IssueIdentity, tt.wantIdentity)
			}
			if !slices.Equal(tracker.fetchIssueByIDCalls, tt.wantReads) {
				t.Errorf("FetchIssueByID called with %v, want %v", tracker.fetchIssueByIDCalls, tt.wantReads)
			}
			var rows []IssueIdentity
			for _, row := range store.parkedIssues {
				rows = append(rows, IssueIdentity{Title: row.IssueTitle, URL: row.IssueURL, IssueType: row.IssueType, Labels: row.IssueLabels})
			}
			if !slices.EqualFunc(rows, tt.wantRows, sameStoredIdentity) {
				t.Errorf("upserted park identities = %+v, want %+v", rows, tt.wantRows)
			}
		})
	}
}
