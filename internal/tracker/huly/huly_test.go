package huly

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

var (
	testActiveStates   = []string{"Todo", "In Progress"}
	testTerminalStates = []string{"Done", "Canceled"}
)

func newTestAdapter(t *testing.T, f *fakeRPC) *HulyAdapter {
	t.Helper()

	a, err := newAdapter(context.Background(), f, "HULY", "social-bot", "http://huly.test/workbench/ws/tracker/",
		testActiveStates, append(slices.Clone(testActiveStates), testTerminalStates...),
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newAdapter() error = %v", err)
	}
	return a
}

func trackerKind(err error) domain.TrackerErrorKind {
	if te, ok := errors.AsType[*domain.TrackerError](err); ok {
		return te.Kind
	}
	return ""
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	meta, ok := registry.Trackers.Meta("huly")
	if !ok {
		t.Fatal(`Trackers.Meta("huly") reported not registered`)
	}
	if !meta.RequiresProject || !meta.RequiresAPIKey {
		t.Errorf("meta = %+v, want RequiresProject and RequiresAPIKey", meta)
	}
	if meta.BlockerSource != registry.BlockersFromCandidates {
		t.Errorf("BlockerSource = %v, want BlockersFromCandidates", meta.BlockerSource)
	}
}

func TestNewAdapterRejectsUnknownState(t *testing.T) {
	t.Parallel()

	_, err := newAdapter(context.Background(), seedWorkspace(), "HULY", "", "", []string{"Todo"}, []string{"Todo", "Review"},
		slog.New(slog.DiscardHandler))
	if trackerKind(err) != domain.ErrTrackerPayload || !strings.Contains(err.Error(), "Review") {
		t.Fatalf("newAdapter() error = %v, want payload error naming Review", err)
	}
}

func TestNewAdapterRejectsUnknownProject(t *testing.T) {
	t.Parallel()

	_, err := newAdapter(context.Background(), seedWorkspace(), "NOPE", "", "", nil, nil,
		slog.New(slog.DiscardHandler))
	if trackerKind(err) != domain.ErrTrackerNotFound {
		t.Fatalf("newAdapter() error = %v, want not found", err)
	}
}

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, seedWorkspace())
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}

	byID := map[string]domain.Issue{}
	for _, i := range issues {
		byID[i.Identifier] = i
	}
	if len(issues) != 2 || byID["HULY-1"].ID == "" || byID["HULY-3"].ID == "" {
		t.Fatalf("candidates = %+v, want HULY-1 and HULY-3", issues)
	}

	first := byID["HULY-1"]
	if first.State != "Todo" || first.Assignee != "Ada Lovelace" || first.Priority == nil || *first.Priority != 2 {
		t.Errorf("HULY-1 = %+v, want state Todo, assignee Ada Lovelace, priority 2", first)
	}
	if !slices.Equal(first.Labels, []string{"bug"}) {
		t.Errorf("HULY-1 labels = %v, want [bug]", first.Labels)
	}
	if len(first.BlockedBy) != 1 || first.BlockedBy[0].Identifier != "HULY-2" || first.BlockedBy[0].State != "Done" {
		t.Errorf("HULY-1 blocked_by = %+v, want HULY-2 in Done", first.BlockedBy)
	}
	if first.Parent != nil || first.Comments != nil || first.Description != "" {
		t.Errorf("HULY-1 parent=%v comments=%v description=%q, want none", first.Parent, first.Comments, first.Description)
	}
	if first.URL != "http://huly.test/workbench/ws/tracker/HULY-1" || first.CreatedAt != "2023-11-14T22:13:20Z" {
		t.Errorf("HULY-1 url=%q created=%q", first.URL, first.CreatedAt)
	}

	child := byID["HULY-3"]
	if child.Parent == nil || child.Parent.ID != "issue1" || child.Parent.Identifier != "HULY-1" {
		t.Errorf("HULY-3 parent = %+v, want issue1/HULY-1", child.Parent)
	}
	if child.Priority != nil || child.Labels == nil || child.BlockedBy == nil {
		t.Errorf("HULY-3 = %+v, want nil priority and non-nil empty labels and blockers", child)
	}
}

func TestFetchIssueByID(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, seedWorkspace())
	for _, id := range []string{"issue1", "HULY-1"} {
		issue, err := a.FetchIssueByID(context.Background(), id)
		if err != nil {
			t.Fatalf("FetchIssueByID(%q) error = %v", id, err)
		}
		if issue.ID != "issue1" || issue.Identifier != "HULY-1" || issue.URL != "http://huly.test/workbench/ws/tracker/HULY-1" {
			t.Errorf("FetchIssueByID(%q) id=%q identifier=%q url=%q", id, issue.ID, issue.Identifier, issue.URL)
		}
		if issue.Description != "Fix **it**" {
			t.Errorf("description = %q, want %q", issue.Description, "Fix **it**")
		}
		if len(issue.Comments) != 2 || issue.Comments[0].Body != "first" || issue.Comments[1].Body != "second" {
			t.Fatalf("comments = %+v, want first then second", issue.Comments)
		}
		if issue.Comments[1].Author != "Ada Lovelace" || issue.Comments[0].Author != "" {
			t.Errorf("authors = %q, %q", issue.Comments[0].Author, issue.Comments[1].Author)
		}
	}

	_, err := a.FetchIssueByID(context.Background(), "HULY-99")
	if trackerKind(err) != domain.ErrTrackerNotFound {
		t.Errorf("FetchIssueByID(missing) error = %v, want not found", err)
	}
}

func TestFetchIssueStates(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, seedWorkspace())

	byIDs, err := a.FetchIssueStatesByIDs(context.Background(), []string{"issue1", "issue2", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byIDs) != 2 || byIDs["issue1"] != "Todo" || byIDs["issue2"] != "Done" {
		t.Errorf("FetchIssueStatesByIDs() = %v", byIDs)
	}

	byKeys, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"HULY-3"})
	if err != nil {
		t.Fatal(err)
	}
	if byKeys["HULY-3"] != "In Progress" {
		t.Errorf("FetchIssueStatesByIdentifiers() = %v", byKeys)
	}

	byStates, err := a.FetchIssuesByStates(context.Background(), []string{"done"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byStates) != 1 || byStates[0].Identifier != "HULY-2" {
		t.Errorf("FetchIssuesByStates(done) = %+v", byStates)
	}
}

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)

	if err := a.TransitionIssue(context.Background(), "HULY-1", "in progress"); err != nil {
		t.Fatalf("TransitionIssue() error = %v", err)
	}
	txs := f.txsOf(classIssue)
	if len(txs) != 1 {
		t.Fatalf("issue txs = %d, want 1", len(txs))
	}
	tx := txs[0]
	if tx["_class"] != classTxUpdateDoc || tx["objectId"] != "issue1" || tx["objectSpace"] != "proj1" || tx["modifiedBy"] != "social-bot" {
		t.Errorf("tx = %+v", tx)
	}
	if ops := tx["operations"].(map[string]any); ops["status"] != "st-progress" {
		t.Errorf("operations = %+v, want status st-progress", ops)
	}

	if err := a.TransitionIssue(context.Background(), "HULY-1", "In Progress"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.txsOf(classIssue)); n != 1 {
		t.Errorf("a converged transition sent a write; issue txs = %d", n)
	}

	if err := a.TransitionIssue(context.Background(), "HULY-1", "Shipped"); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("TransitionIssue(unknown) error = %v, want payload error", err)
	}
}

func TestCommentIssue(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)

	if err := a.CommentIssueWithLiteral(context.Background(), "HULY-1", "Session stopped.", "see `x`"); err != nil {
		t.Fatalf("CommentIssueWithLiteral() error = %v", err)
	}
	txs := f.txsOf(classChatMessage)
	if len(txs) != 1 {
		t.Fatalf("message txs = %d, want 1", len(txs))
	}
	tx := txs[0]
	if tx["_class"] != classTxCreateDoc || tx["attachedTo"] != "issue1" || tx["collection"] != collectionNotes ||
		tx["attachedToClass"] != classIssue || tx["createdBy"] != "social-bot" {
		t.Errorf("tx = %+v", tx)
	}

	comments, err := a.FetchIssueComments(context.Background(), "issue1")
	if err != nil {
		t.Fatal(err)
	}
	if got := comments[len(comments)-1].Body; got != "Session stopped.\n\n```\nsee `x`\n```" {
		t.Errorf("posted comment renders as %q", got)
	}
}

func TestLabels(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)
	ctx := context.Background()

	if err := a.AddLabel(ctx, "HULY-1", "BUG"); err != nil {
		t.Fatalf("AddLabel(existing) error = %v", err)
	}
	if n := len(f.txsOf(classTagReference)); n != 0 {
		t.Errorf("adding a carried label wrote %d references", n)
	}

	if err := a.AddLabel(ctx, "HULY-1", "stage-plan"); err != nil {
		t.Fatalf("AddLabel(new) error = %v", err)
	}
	if n := len(f.txsOf(classTagElement)); n != 1 {
		t.Errorf("tag element creates = %d, want 1", n)
	}
	issue, err := a.FetchIssueByID(ctx, "HULY-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(issue.Labels, "stage-plan") || !slices.Contains(issue.Labels, "bug") {
		t.Errorf("labels after add = %v", issue.Labels)
	}

	if err := a.RemoveLabel(ctx, "HULY-1", "Bug"); err != nil {
		t.Fatalf("RemoveLabel() error = %v", err)
	}
	issue, err = a.FetchIssueByID(ctx, "HULY-1")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(issue.Labels, "bug") {
		t.Errorf("labels after remove = %v", issue.Labels)
	}

	if err := a.AddLabel(ctx, "HULY-1", "  "); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("AddLabel(blank) error = %v, want payload error", err)
	}
}

func TestLooksLikeIdentifier(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]bool{
		"HULY-12": true, "A-1": true, "65f1c0de9a1b2c3d4e5f6a7b": false, "HULY-": false, "-3": false, "HULY-x1": false,
	} {
		if got := looksLikeIdentifier(in); got != want {
			t.Errorf("looksLikeIdentifier(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCandidatesFollowReloadedStates(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, seedWorkspace())
	a.SetTrackerStates(func() domain.TrackerStates { return domain.TrackerStates{Active: []string{"Done"}} })
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil || len(issues) != 1 || issues[0].Identifier != "HULY-2" {
		t.Fatalf("FetchCandidateIssues() = %+v, %v; want only the Done issue HULY-2", issues, err)
	}
}
