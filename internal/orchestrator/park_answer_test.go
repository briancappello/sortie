package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/parkanswer"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/workspace"
)

const (
	answerIssueID    = "id-ons-25"
	answerIdentifier = "ONS-25"
	answerStatement  = "Which repository should the daemon live in?"
)

// parkStoryBlocked parks identifier the way a run whose agent wrote
// "blocked" with statement does, and confirms the parking label.
func parkStoryBlocked(t *testing.T, store *persistence.Store, issueID, identifier, statement string) *State {
	t.Helper()
	state := exitStateWithIssue(t, issueID, "In Progress")
	running := state.Running[issueID]
	running.Identifier, running.SessionID, running.AgentKind = identifier, "sess-parked", "mock"

	HandleWorkerExit(state, WorkerResult{
		IssueID:           issueID,
		Identifier:        identifier,
		ExitKind:          WorkerExitNormal,
		AgentAdapter:      "mock",
		SoftStop:          true,
		SoftStopReason:    "blocked",
		SoftStopStatement: workspace.StatusStatement{Text: statement},
	}, HandleWorkerExitParams{
		Store:               store,
		MaxRetryBackoffMS:   300_000,
		OnRetryFire:         noopRetryFire,
		NowFunc:             func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:              discardLogger(),
		HandoffParkingLabel: "needs-human",
	})
	state.TrackerOpsWg.Wait()
	if _, ok := state.Parked[issueID]; !ok {
		t.Fatalf("issue %s is not parked after its run wrote blocked", identifier)
	}
	state.Parked[issueID].LabelApplied = true
	if err := store.MarkParkedIssueLabelApplied(context.Background(), issueID); err != nil {
		t.Fatalf("MarkParkedIssueLabelApplied: %v", err)
	}
	return state
}

// commentingTracker is a fake tracker whose comments are those posted
// through it, each created when it was posted.
type commentingTracker struct {
	*candidateTrackerAdapter

	mu       sync.Mutex
	comments map[string][]domain.Comment
	posts    []commentIssueCall
	postErr  error
}

func newCommentingTracker(candidates ...domain.Issue) *commentingTracker {
	ct := &commentingTracker{comments: map[string][]domain.Comment{}}
	ct.candidateTrackerAdapter = &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{fetchCommentsFn: func(_ context.Context, id string) ([]domain.Comment, error) {
			ct.mu.Lock()
			defer ct.mu.Unlock()
			return append([]domain.Comment{}, ct.comments[id]...), nil
		}},
		fetchCandidatesFn: func(context.Context) ([]domain.Issue, error) { return candidates, nil },
	}
	return ct
}

func (ct *commentingTracker) CommentIssue(_ context.Context, issueID, text string) error {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.posts = append(ct.posts, commentIssueCall{IssueID: issueID, Text: text})
	if ct.postErr != nil {
		return ct.postErr
	}
	ct.comments[issueID] = append(ct.comments[issueID], domain.Comment{
		ID: "c-answer", Author: "sortie-bot", Body: text, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

func (ct *commentingTracker) postCalls() []commentIssueCall {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return append([]commentIssueCall{}, ct.posts...)
}

const answerTemplate = `{{ if .park }}gesture={{ .park.gesture }}|statement={{ .park.statement }}|` +
	`{{ range .park.comments }}comment={{ .body }};{{ end }}{{ else }}no-park{{ end }}`

type answerHarness struct {
	orch    *Orchestrator
	state   *State
	store   *persistence.Store
	tracker *commentingTracker
	resumes chan string
	prompts chan string
}

func newAnswerHarness(t *testing.T, state *State, store *persistence.Store, tracker *commentingTracker) *answerHarness {
	t.Helper()
	h := &answerHarness{state: state, store: store, tracker: tracker, resumes: make(chan string, 1), prompts: make(chan string, 1)}
	wm := &stubWorkflowManager{config: defaultWorkerConfig(t.TempDir()), template: mustParseTemplate(t, answerTemplate)}
	wm.config.Tracker.Kind, wm.config.Tracker.APIKey = "mock", "key"
	wm.config.Agent.MaxTurns = 1
	wm.config.Agent.MaxConcurrentAgents = 10
	agent := &mockAgentAdapter{
		startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
			h.resumes <- params.ResumeSessionID
			return domain.Session{ID: "sess-new"}, nil
		},
		runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
			h.prompts <- params.Prompt
			return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
		},
	}
	regs := passingPreflightRegistries()
	regs.ReloadWorkflow = func() error { return nil }
	regs.ConfigFunc = wm.Config
	h.orch = NewOrchestrator(OrchestratorParams{
		State: state, Logger: discardLogger(), TrackerAdapter: tracker, Router: route.NewRouter(tracker, nil),
		AgentAdapter: agent, WorkflowManager: wm, Store: store, PreflightParams: regs,
	})
	t.Cleanup(state.WorkerWg.Wait)
	return h
}

func (h *answerHarness) answer(identifier, text, author string) ControlReply {
	return h.orch.handleControl(context.Background(), controlRequest{
		Kind: controlAnswer, Identifier: identifier, Answer: parkanswer.Answer{Text: text, Author: author},
	})
}

// Scenario: an answer resumes the story.
func TestAnswerResumesParkedStory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openParkStore(t)
	state := parkStoryBlocked(t, store, answerIssueID, answerIdentifier, answerStatement)
	tracker := newCommentingTracker(domain.Issue{
		ID: answerIssueID, Identifier: answerIdentifier, Title: "Sortie daemon", State: "In Progress", Labels: []string{"needs-human"},
	})
	h := newAnswerHarness(t, state, store, tracker)

	reply := h.answer(answerIdentifier, "agentic-orchestration", "Serghei")

	if reply.Outcome != ControlDone {
		t.Fatalf("answer outcome = %v (%s), want ControlDone", reply.Outcome, reply.Message)
	}
	if reply.Answer.Statement != answerStatement || reply.Answer.Identifier != answerIdentifier || reply.Answer.Author != "Serghei" || reply.Answer.ReleasedAt.IsZero() {
		t.Errorf("answer reply = %+v, want the park's statement, identifier, author, and release time", reply.Answer)
	}
	posts := tracker.postCalls()
	wantComment := "Serghei answered through Sortie:\n\nagentic-orchestration"
	if len(posts) != 1 || posts[0].IssueID != answerIssueID || posts[0].Text != wantComment {
		t.Fatalf("comments posted = %+v, want one on %s attributed to Serghei: %q", posts, answerIssueID, wantComment)
	}
	if _, parked := state.Parked[answerIssueID]; parked {
		t.Error("the issue is still parked after the answer")
	}
	if release := state.ParkReleases[answerIssueID]; release == nil || release.ReleaseGesture != "answered" {
		t.Fatalf("pending release = %+v, want one with gesture answered", release)
	}
	rows, err := store.ListParkedIssues(ctx)
	if err != nil || len(rows) != 1 || rows[0].ReleasedAt == "" || rows[0].ReleaseGesture != "answered" {
		t.Fatalf("parked_issues after the answer = %+v, %v, want one row released by answered", rows, err)
	}

	h.orch.handleTick(ctx)

	if got := waittest.Receive(t, h.resumes, "session start"); got != "sess-parked" {
		t.Errorf("ResumeSessionID of the next run = %q, want the parked session sess-parked", got)
	}
	prompt := waittest.Receive(t, h.prompts, "first turn")
	for _, want := range []string{"gesture=answered", "statement=" + answerStatement, "comment=" + wantComment + ";"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("first-turn prompt = %q, want it to hold %q", prompt, want)
		}
	}
	if rows, err := store.ListParkedIssues(ctx); err != nil || len(rows) != 0 {
		t.Errorf("parked_issues after the dispatch = %+v, %v, want none", rows, err)
	}
}

// Scenario: not parked.
func TestAnswerNotParked(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, store *persistence.Store) *State
	}{
		{"running issue", func(t *testing.T, _ *persistence.Store) *State {
			state := exitStateWithIssue(t, answerIssueID, "In Progress")
			state.Running[answerIssueID].Identifier = answerIdentifier
			return state
		}},
		{"issue Sortie does not know", func(t *testing.T, _ *persistence.Store) *State {
			return NewState(1000, 10, 0, nil, AgentTotals{})
		}},
		{"park already released, waiting for its run", func(t *testing.T, store *persistence.Store) *State {
			state := parkStoryBlocked(t, store, answerIssueID, answerIdentifier, answerStatement)
			unparkIssue(context.Background(), state, answerIssueID, unparkTriggerLabelRemoved, store, discardLogger())
			return state
		}},
		{"another issue is parked", func(t *testing.T, store *persistence.Store) *State {
			state := parkStoryBlocked(t, store, "id-ons-26", "ONS-26", "Which port?")
			state.Running[answerIssueID] = &RunningEntry{Identifier: answerIdentifier, StartedAt: baseTime}
			return state
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := openParkStore(t)
			state := tt.setup(t, store)
			rowsBefore, err := store.ListParkedIssues(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			tracker := newCommentingTracker()
			h := newAnswerHarness(t, state, store, tracker)

			reply := h.answer(answerIdentifier, "agentic-orchestration", "Serghei")

			if reply.Outcome != ControlNotParked {
				t.Fatalf("answer outcome = %v (%s), want ControlNotParked", reply.Outcome, reply.Message)
			}
			if posts := tracker.postCalls(); len(posts) != 0 {
				t.Errorf("comments posted = %+v, want none", posts)
			}
			rowsAfter, err := store.ListParkedIssues(context.Background())
			if err != nil || len(rowsAfter) != len(rowsBefore) {
				t.Fatalf("parked_issues = %+v, %v, want the %d rows from before", rowsAfter, err, len(rowsBefore))
			}
			for i := range rowsAfter {
				if rowsAfter[i].ReleasedAt != rowsBefore[i].ReleasedAt || rowsAfter[i].ReleaseGesture != rowsBefore[i].ReleaseGesture {
					t.Errorf("parked_issues row %d = %+v, want it unchanged from %+v", i, rowsAfter[i], rowsBefore[i])
				}
			}
			if other := state.Parked["id-ons-26"]; tt.name == "another issue is parked" && other == nil {
				t.Error("the other parked issue lost its park")
			}
		})
	}
}

func TestAnswerKeepsParkWhenNothingIsPosted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		text        string
		author      string
		postErr     error
		wantOutcome ControlOutcome
		wantPosts   int
	}{
		{"comment write fails", "agentic-orchestration", "Serghei", errors.New("tracker unavailable"), ControlTrackerFailed, 1},
		{"blank text", "  \n", "Serghei", nil, ControlInvalid, 0},
		{"blank author", "agentic-orchestration", " ", nil, ControlInvalid, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := openParkStore(t)
			state := parkStoryBlocked(t, store, answerIssueID, answerIdentifier, answerStatement)
			tracker := newCommentingTracker()
			tracker.postErr = tt.postErr
			h := newAnswerHarness(t, state, store, tracker)

			reply := h.answer(answerIdentifier, tt.text, tt.author)

			if reply.Outcome != tt.wantOutcome {
				t.Fatalf("answer outcome = %v (%s), want %v", reply.Outcome, reply.Message, tt.wantOutcome)
			}
			if posts := tracker.postCalls(); len(posts) != tt.wantPosts {
				t.Errorf("comment posts = %+v, want %d", posts, tt.wantPosts)
			}
			if state.Parked[answerIssueID] == nil || state.ParkReleases[answerIssueID] != nil {
				t.Error("the park was released, want it kept")
			}
			if rows, err := store.ListParkedIssues(context.Background()); err != nil || len(rows) != 1 || rows[0].ReleasedAt != "" {
				t.Errorf("parked_issues = %+v, %v, want the one unreleased row", rows, err)
			}
		})
	}
}

func TestSnapshotParkedCarriesStatement(t *testing.T) {
	t.Parallel()

	statement := answerStatement + " " + strings.Repeat("It could live in sortie or in agentic-orchestration. ", 15)
	state := parkStoryBlocked(t, openParkStore(t), answerIssueID, answerIdentifier, statement)

	parked := RuntimeSnapshot(state, baseTime).Parked
	if len(parked) != 1 || parked[0].Statement != strings.TrimSpace(statement) {
		t.Errorf("snapshot parked = %+v, want one row with the stop statement in full", parked)
	}
}
