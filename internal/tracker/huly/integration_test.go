package huly

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessIntegration skips the current test when SORTIE_HULY_TEST is not
// "1", so disabled integration tests report as skipped.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_HULY_TEST") != "1" {
		t.Skip("skipping Huly integration test: set SORTIE_HULY_TEST=1 to enable")
	}
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// integrationConfig is the adapter configuration from SORTIE_HULY_URL,
// SORTIE_HULY_TOKEN, SORTIE_HULY_WORKSPACE and SORTIE_HULY_PROJECT, plus
// the plan task types from SORTIE_HULY_EPIC_TYPE and SORTIE_HULY_STORY_TYPE
// (default Epic and Story), and the classic workflow's state lists.
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()

	return map[string]any{
		"endpoint":   requireEnv(t, "SORTIE_HULY_URL"),
		"api_key":    requireEnv(t, "SORTIE_HULY_TOKEN"),
		"workspace":  requireEnv(t, "SORTIE_HULY_WORKSPACE"),
		"project":    requireEnv(t, "SORTIE_HULY_PROJECT"),
		"epic_type":  envOr("SORTIE_HULY_EPIC_TYPE", "Epic"),
		"story_type": envOr("SORTIE_HULY_STORY_TYPE", "Story"),

		"active_states":   []string{"Todo", "In Progress"},
		"terminal_states": []string{"Done", "Canceled"},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// integrationAdapter builds an adapter from integrationConfig.
func integrationAdapter(t *testing.T) *HulyAdapter {
	t.Helper()

	adapter, err := NewHulyAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewHulyAdapter() error = %v", err)
	}
	return adapter.(*HulyAdapter)
}

// TestIntegrationPlanTypes resolves the configured epic and story task
// types, and checks that an unknown type fails construction with the
// project's task type names.
func TestIntegrationPlanTypes(t *testing.T) {
	skipUnlessIntegration(t)

	a := integrationAdapter(t)
	if !a.plan.configured() {
		t.Fatal("plan task types not resolved")
	}
	t.Logf("epic=%s (%s) story=%s (%s)", a.plan.epic.Name, a.plan.epic.ID, a.plan.story.Name, a.plan.story.ID)

	cfg := integrationConfig(t)
	cfg["epic_type"] = "No Such Type"
	_, err := NewHulyAdapter(cfg)
	if err == nil || !strings.Contains(err.Error(), "No Such Type") || !strings.Contains(err.Error(), a.plan.story.Name) {
		t.Fatalf("NewHulyAdapter(unknown epic type) error = %v, want one naming the type and the project's task types", err)
	}
	t.Logf("unknown type: %v", err)
}

// TestIntegrationReadWrite exercises every operation against a live
// workspace. SORTIE_HULY_ISSUE names an issue it may comment on, label,
// and move between states; the issue's original state is restored.
func TestIntegrationReadWrite(t *testing.T) {
	skipUnlessIntegration(t)

	a := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	identifier := requireEnv(t, "SORTIE_HULY_ISSUE")

	candidates, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	t.Logf("%d candidates", len(candidates))

	issue, err := a.FetchIssueByID(ctx, identifier)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s) error = %v", identifier, err)
	}
	t.Logf("%s %q state=%s labels=%v assignee=%q comments=%d\n%s",
		issue.Identifier, issue.Title, issue.State, issue.Labels, issue.Assignee, len(issue.Comments), issue.Description)

	byID, err := a.FetchIssueByID(ctx, issue.ID)
	if err != nil || byID.Identifier != identifier {
		t.Fatalf("FetchIssueByID(%s) = %s, %v", issue.ID, byID.Identifier, err)
	}

	original := issue.State
	target := "Todo"
	if strings.EqualFold(original, target) {
		target = "In Progress"
	}
	if err := a.TransitionIssue(ctx, identifier, target); err != nil {
		t.Fatalf("TransitionIssue(%s) error = %v", target, err)
	}
	states, err := a.FetchIssueStatesByIdentifiers(ctx, []string{identifier})
	if err != nil || !strings.EqualFold(states[identifier], target) {
		t.Errorf("state after transition = %v, %v; want %s", states, err, target)
	}
	if err := a.TransitionIssue(ctx, identifier, original); err != nil {
		t.Errorf("TransitionIssue(restore %s) error = %v", original, err)
	}

	stamp := fmt.Sprintf("sortie integration %d", time.Now().UnixNano())
	if err := a.CommentIssueWithLiteral(ctx, identifier, stamp, "literal `block` [x](y)"); err != nil {
		t.Fatalf("CommentIssueWithLiteral() error = %v", err)
	}
	comments, err := a.FetchIssueComments(ctx, identifier)
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}
	if len(comments) == 0 || !strings.HasPrefix(comments[len(comments)-1].Body, stamp) {
		t.Errorf("last comment = %+v, want it to start with %q", comments[len(comments)-1:], stamp)
	} else {
		t.Logf("posted comment reads back as %q by %q", comments[len(comments)-1].Body, comments[len(comments)-1].Author)
	}

	label := "sortie-integration"
	if err := a.AddLabel(ctx, identifier, label); err != nil {
		t.Fatalf("AddLabel() error = %v", err)
	}
	labeled, err := a.FetchIssueByID(ctx, identifier)
	if err != nil || !slices.Contains(labeled.Labels, label) {
		t.Errorf("labels after add = %v, %v", labeled.Labels, err)
	}
	if err := a.RemoveLabel(ctx, identifier, label); err != nil {
		t.Fatalf("RemoveLabel() error = %v", err)
	}
	unlabeled, err := a.FetchIssueByID(ctx, identifier)
	if err != nil || slices.Contains(unlabeled.Labels, label) {
		t.Errorf("labels after remove = %v, %v", unlabeled.Labels, err)
	}
}

// TestIntegrationPlanKey creates the plan attributes (once), stores a
// probe plan key on SORTIE_HULY_ISSUE, finds the issue by it before and
// after a rename, and then clears the key and restores the title.
func TestIntegrationPlanKey(t *testing.T) {
	skipUnlessIntegration(t)

	a := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for range 2 {
		if err := integrationAdapter(t).ensurePlanAttributes(ctx); err != nil {
			t.Fatalf("ensurePlanAttributes() error = %v", err)
		}
	}
	raws, err := a.rpc.FindAll(ctx, classAttribute, map[string]any{
		"attributeOf": idIn([]string{classIssue}),
		"name":        idIn([]string{attrPlanKey, attrPlanBodyDigest}),
	}, nil)
	if err != nil || len(raws) != 2 {
		t.Fatalf("plan attributes = %d, %v; want exactly 2", len(raws), err)
	}

	issue, err := a.findIssue(ctx, requireEnv(t, "SORTIE_HULY_ISSUE"))
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("sortie-integration/probe-%d", time.Now().UnixNano())
	update := func(ctx context.Context, operations map[string]any) error {
		tx := a.txBase(classIssue, classTxUpdateDoc, issue.Space, issue.ID)
		tx["operations"] = operations
		return a.rpc.Tx(ctx, tx)
	}
	setTitle := func(title string) {
		t.Helper()
		if err := update(ctx, map[string]any{"title": title}); err != nil {
			t.Fatal(err)
		}
	}
	if err := update(ctx, map[string]any{attrPlanKey: key}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = update(context.Background(), map[string]any{attrPlanKey: "", "title": issue.Title})
	}()

	found, err := a.FindIssueByPlanKey(ctx, key)
	if err != nil || found.ID != issue.ID || found.PlanKey != key {
		t.Fatalf("FindIssueByPlanKey() = %+v, %v; want %s", found, err, issue.Identifier)
	}
	setTitle(issue.Title + " (renamed)")
	found, err = a.FindIssueByPlanKey(ctx, key)
	if err != nil || found.ID != issue.ID || !strings.HasSuffix(found.Title, "(renamed)") {
		t.Fatalf("FindIssueByPlanKey() after rename = %+v, %v", found, err)
	}
	keyed, err := a.ListPlanIssues(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(keyed, func(p domain.PlanIssue) bool { return p.PlanKey == key }) {
		t.Errorf("ListPlanIssues() = %+v, want it to hold %s", keyed, key)
	}
}

// TestIntegrationCreatePlanIssues creates an epic and a story under it,
// reads them back by plan key, and removes them unless SORTIE_HULY_KEEP
// is "1", which leaves them for a look in Huly's UI.
func TestIntegrationCreatePlanIssues(t *testing.T) {
	skipUnlessIntegration(t)

	a := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	prefix := fmt.Sprintf("sortie-integration-%d", time.Now().Unix())
	epic, err := a.CreateIssue(ctx, domain.NewIssue{
		Title: "Integration epic " + prefix, Kind: domain.IssueKindEpic, PlanKey: prefix,
		Body: "OpenSpec change `" + prefix + "`.\n\n## Why\n\nChecks plan writes.",
	})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	story, err := a.CreateIssue(ctx, domain.NewIssue{
		Title: "Integration story", Kind: domain.IssueKindStory, PlanKey: prefix + "/story", ParentID: epic.ID,
		Body: "## Acceptance criteria\n\n- It nests under the epic.\n- It has the Story type.",
	})
	if err != nil {
		t.Fatalf("CreateIssue(story) error = %v", err)
	}
	t.Logf("created %s (epic) and %s (story)", epic.Identifier, story.Identifier)
	if os.Getenv("SORTIE_HULY_KEEP") != "1" {
		defer func() {
			for _, id := range []string{story.ID, epic.ID} {
				_ = a.rpc.Tx(context.Background(), a.removeTx(classIssue, a.project.ID, id))
			}
		}()
	}

	got, err := a.FindIssueByPlanKey(ctx, prefix+"/story")
	if err != nil || got.ID != story.ID || got.ParentID != epic.ID || got.Kind != domain.IssueKindStory {
		t.Fatalf("FindIssueByPlanKey(story) = %+v, %v", got, err)
	}
	full, err := a.FetchIssueByID(ctx, story.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	if full.Parent == nil || full.Parent.ID != epic.ID || !strings.Contains(full.Description, "It nests under the epic.") {
		t.Errorf("story = parent %+v description %q, want the epic and the body", full.Parent, full.Description)
	}
	e, err := a.findIssue(ctx, epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("epic kind=%s rank=%s; story parent=%s; description:\n%s", e.Kind, e.Rank, full.Parent.Identifier, full.Description)
}

// TestIntegrationPlanEdits creates an epic and two stories, then links,
// relinks, and unlinks a dependency, updates a story's title and body,
// and moves it out from under the epic and back. The blocked story is
// moved to an active state so the candidate listing must report the new
// blocker. The issues are removed unless SORTIE_HULY_KEEP is "1".
func TestIntegrationPlanEdits(t *testing.T) {
	skipUnlessIntegration(t)

	a := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prefix := fmt.Sprintf("sortie-integration-%d", time.Now().Unix())
	create := func(in domain.NewIssue) domain.CreatedIssue {
		t.Helper()
		c, err := a.CreateIssue(ctx, in)
		if err != nil {
			t.Fatalf("CreateIssue(%s) error = %v", in.PlanKey, err)
		}
		return c
	}
	epic := create(domain.NewIssue{Title: "Edits epic " + prefix, Kind: domain.IssueKindEpic, PlanKey: prefix})
	first := create(domain.NewIssue{Title: "First story", Kind: domain.IssueKindStory, PlanKey: prefix + "/first", ParentID: epic.ID})
	second := create(domain.NewIssue{Title: "Second story", Kind: domain.IssueKindStory, PlanKey: prefix + "/second", ParentID: epic.ID, Body: "- before"})
	t.Logf("epic %s, stories %s and %s", epic.Identifier, first.Identifier, second.Identifier)
	if os.Getenv("SORTIE_HULY_KEEP") != "1" {
		defer func() {
			for _, id := range []string{second.ID, first.ID, epic.ID} {
				_ = a.rpc.Tx(context.Background(), a.removeTx(classIssue, a.project.ID, id))
			}
		}()
	}

	for range 2 {
		if err := a.AddDependency(ctx, first.ID, second.ID); err != nil {
			t.Fatalf("AddDependency() error = %v", err)
		}
	}
	if err := a.RemoveDependency(ctx, second.ID, first.ID); err != nil {
		t.Fatalf("RemoveDependency(missing) error = %v", err)
	}
	got, err := a.FindIssueByPlanKey(ctx, prefix+"/second")
	if err != nil || !slices.Equal(got.BlockedBy, []string{first.ID}) {
		t.Fatalf("second blockedBy = %v, %v; want exactly the first story", got.BlockedBy, err)
	}

	if err := a.TransitionIssue(ctx, second.ID, a.TrackerStates().Active[0]); err != nil {
		t.Fatal(err)
	}
	candidates, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	idx := slices.IndexFunc(candidates, func(i domain.Issue) bool { return i.ID == second.ID })
	if idx < 0 || len(candidates[idx].BlockedBy) != 1 || candidates[idx].BlockedBy[0].Identifier != first.Identifier {
		t.Fatalf("candidate %s = %+v, want it blocked by %s", second.Identifier, candidates[idx:idx+1], first.Identifier)
	}

	title, body := "Second story, renamed", "- after\n- and more"
	if err := a.UpdateIssue(ctx, second.ID, domain.IssueFields{Title: &title, Body: &body}); err != nil {
		t.Fatalf("UpdateIssue() error = %v", err)
	}
	full, err := a.FetchIssueByID(ctx, second.ID)
	if err != nil || full.Title != title || !strings.Contains(full.Description, "and more") {
		t.Errorf("after update: %q %q, %v", full.Title, full.Description, err)
	}

	if err := a.SetParent(ctx, second.ID, ""); err != nil {
		t.Fatalf("SetParent(none) error = %v", err)
	}
	if got, _ := a.FindIssueByPlanKey(ctx, prefix+"/second"); got.ParentID != "" {
		t.Errorf("parent after removal = %q, want none", got.ParentID)
	}
	if err := a.SetParent(ctx, second.ID, epic.ID); err != nil {
		t.Fatalf("SetParent(epic) error = %v", err)
	}
	if got, _ := a.FindIssueByPlanKey(ctx, prefix+"/second"); got.ParentID != epic.ID {
		t.Errorf("parent after restore = %q, want the epic", got.ParentID)
	}

	if err := a.RemoveDependency(ctx, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.FindIssueByPlanKey(ctx, prefix+"/second"); len(got.BlockedBy) != 0 {
		t.Errorf("blockedBy after unlink = %v, want empty", got.BlockedBy)
	}
}
