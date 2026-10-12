package huly

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
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
// (default Epic and Story).
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()

	return map[string]any{
		"endpoint":   requireEnv(t, "SORTIE_HULY_URL"),
		"api_key":    requireEnv(t, "SORTIE_HULY_TOKEN"),
		"workspace":  requireEnv(t, "SORTIE_HULY_WORKSPACE"),
		"project":    requireEnv(t, "SORTIE_HULY_PROJECT"),
		"epic_type":  envOr("SORTIE_HULY_EPIC_TYPE", "Epic"),
		"story_type": envOr("SORTIE_HULY_STORY_TYPE", "Story"),
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
