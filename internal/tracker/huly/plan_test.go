package huly

import (
	"context"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

var classicStatuses = []string{"st-backlog", "st-todo", "st-progress", "st-done", "st-canceled"}

// seedTaskTypes adds the Issue, Epic and Story task types to project type
// ptype1 of seedWorkspace.
func seedTaskTypes(f *fakeRPC) {
	for id, name := range map[string]string{"tt-issue": "Issue", "tt-epic": "Epic", "tt-story": "Story"} {
		f.add(classTaskType, map[string]any{"_id": id, "name": name, "parent": "ptype1", "statuses": classicStatuses})
	}
}

func TestResolvePlanTypes(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	seedTaskTypes(f)
	a := newTestAdapter(t, f)

	if err := a.resolvePlanTypes(context.Background(), "epic", " Story "); err != nil {
		t.Fatalf("resolvePlanTypes() error = %v", err)
	}
	if a.plan.epic.ID != "tt-epic" || a.plan.story.ID != "tt-story" {
		t.Errorf("plan types = %+v, want tt-epic and tt-story", a.plan)
	}
	if len(a.plan.story.Statuses) != len(classicStatuses) {
		t.Errorf("story statuses = %v, want the type's statuses", a.plan.story.Statuses)
	}
	if got := a.plan.kindOf("tt-story"); got != domain.IssueKindStory {
		t.Errorf("kindOf(tt-story) = %q, want story", got)
	}
	if got := a.plan.kindOf("tt-issue"); got != "" {
		t.Errorf("kindOf(tt-issue) = %q, want empty", got)
	}
}

func TestResolvePlanTypesMissingNamesProjectTypes(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	f.add(classTaskType, map[string]any{"_id": "tt-issue", "name": "Issue", "parent": "ptype1", "statuses": classicStatuses})
	f.add(classTaskType, map[string]any{"_id": "tt-other", "name": "Feature", "parent": "ptype-other", "statuses": classicStatuses})
	a := newTestAdapter(t, f)

	err := a.resolvePlanTypes(context.Background(), "Epic", "Story")
	if trackerKind(err) != domain.ErrTrackerPayload {
		t.Fatalf("resolvePlanTypes() error = %v, want payload error", err)
	}
	for _, want := range []string{`"Epic" (huly.epic_type)`, `"Story" (huly.story_type)`, "its task types are Issue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "Feature") {
		t.Errorf("error %q names a task type of another project type", err)
	}
}

func TestResolvePlanTypesUnconfigured(t *testing.T) {
	t.Parallel()

	a := newTestAdapter(t, seedWorkspace())
	if err := a.resolvePlanTypes(context.Background(), "", ""); err != nil {
		t.Fatalf("resolvePlanTypes() error = %v, want nil when neither is configured", err)
	}
	if a.plan.configured() {
		t.Error("plan types configured without config")
	}
	if _, err := a.plan.forKind(domain.IssueKindEpic); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("forKind() error = %v, want payload error naming the config keys", err)
	}
}

func TestResolvePlanTypesNeedsBoth(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	seedTaskTypes(f)
	a := newTestAdapter(t, f)
	for _, tc := range [][2]string{{"Epic", ""}, {"", "Story"}, {"Epic", "epic"}} {
		if err := a.resolvePlanTypes(context.Background(), tc[0], tc[1]); trackerKind(err) != domain.ErrTrackerPayload {
			t.Errorf("resolvePlanTypes(%q, %q) error = %v, want payload error", tc[0], tc[1], err)
		}
	}
}
