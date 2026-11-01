package plan

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func withRepositories(t Tracker, names ...string) Tracker {
	t.Repositories = func() []string { return names }
	return t
}

// A plan node names its repository.
func TestDecodeReadsStoryRepository(t *testing.T) {
	t.Parallel()

	g := v2(newGraph().epic("billing", ApprovalApproved, "a").build())
	story(&g, "billing/a").Repository = "ao"
	got, err := Decode(encode(t, g))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if r := story(&got, "billing/a").Repository; r != "ao" {
		t.Errorf("repository = %q, want ao", r)
	}
}

func TestDecodeRejectsMisplacedRepository(t *testing.T) {
	t.Parallel()

	g := newGraph().epic("billing", ApprovalApproved, "a").build()
	story(&g, "billing/a").Repository = "ao"
	if msg := decodeErr(t, encode(t, g)); !strings.Contains(msg, "need schema_version 2") {
		t.Errorf("v1 error = %s", msg)
	}

	g = v2(newGraph().epic("billing", ApprovalApproved, "a").build())
	story(&g, "billing").Repository = "ao"
	if msg := decodeErr(t, encode(t, g)); !strings.Contains(msg, "stories only") {
		t.Errorf("epic error = %s", msg)
	}

	g = v2(newGraph().epic("billing", ApprovalApproved, "a").build())
	story(&g, "billing/a").Repository = "Not Valid"
	if msg := decodeErr(t, encode(t, g)); !strings.Contains(msg, "must match") {
		t.Errorf("bad name error = %s", msg)
	}
}

// Plan sync records each story's repository on its issue.
func TestPlanSync_RecordsStoryRepository(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	tr := withRepositories(f.tracker(), "sortie", "ao")
	g := v2(newGraph().epic("billing", ApprovalApproved, "a", "b").build())
	story(&g, "billing/a").Repository = "ao"
	apply(t, tr, g)

	byKey := map[string]*domain.PlanIssue{}
	for _, i := range f.issues {
		byKey[i.PlanKey] = i
	}
	if !slices.Contains(byKey["billing/a"].Labels, RepositoryLabelPrefix+"ao") {
		t.Errorf("billing/a labels = %v, want the ao repository label", byKey["billing/a"].Labels)
	}
	if len(byKey["billing/b"].Labels) != 0 {
		t.Errorf("billing/b labels = %v, want none: it runs in the default", byKey["billing/b"].Labels)
	}

	// A second sync changes nothing; moving the story swaps the label.
	if p := preview(t, g, tr); p.Writes() != 0 {
		t.Errorf("resync writes = %d, want 0 (%+v)", p.Writes(), p.Labels)
	}
	story(&g, "billing/a").Repository = "sortie"
	apply(t, tr, g)
	if l := byKey["billing/a"].Labels; !slices.Equal(l, []string{RepositoryLabelPrefix + "sortie"}) {
		t.Errorf("labels after the move = %v", l)
	}
	story(&g, "billing/a").Repository = ""
	apply(t, tr, g)
	if l := byKey["billing/a"].Labels; len(l) != 0 {
		t.Errorf("labels after dropping the repository = %v, want none", l)
	}
}

// The preview refuses a repository the workflow does not list, and an
// apply of it writes nothing.
func TestPlanSync_RefusesUnlistedRepository(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	tr := withRepositories(f.tracker(), "sortie")
	g := v2(newGraph().epic("billing", ApprovalApproved, "a").build())
	story(&g, "billing/a").Repository = "elsewhere"

	p := preview(t, g, tr)
	if !slices.Equal(p.UnlistedRepositories, []UnlistedRepository{{PlanKey: "billing/a", Repository: "elsewhere"}}) {
		t.Errorf("unlisted = %+v", p.UnlistedRepositories)
	}
	s := NewSyncer(tr)
	_, id, err := s.Preview(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), g, id)
	te, ok := errors.AsType[*domain.TrackerError](err)
	if !ok || te.Kind != domain.ErrTrackerPayload || !strings.Contains(te.Message, "billing/a (elsewhere)") {
		t.Fatalf("Apply() error = %v, want a payload error naming the story", err)
	}
	if len(f.log) != 0 {
		t.Errorf("writes = %v, want none", f.log)
	}
}

func requiringRepository(t Tracker, names ...string) Tracker {
	t = withRepositories(t, names...)
	t.RequiresRepository = func() bool { return true }
	return t
}

// Without a default repository, the preview refuses a story that names
// no repository, by its plan key, and an apply of it writes nothing.
func TestPlanSync_RefusesStoryWithoutRepositoryWhenNoDefault(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	tr := requiringRepository(f.tracker(), "homelab")
	g := v2(newGraph().epic("billing", ApprovalApproved, "a", "b").build())
	story(&g, "billing/b").Repository = "homelab"

	p := preview(t, g, tr)
	if !slices.Equal(p.MissingRepositories, []string{"billing/a"}) {
		t.Errorf("missing = %v, want billing/a", p.MissingRepositories)
	}
	s := NewSyncer(tr)
	_, id, err := s.Preview(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(context.Background(), g, id)
	te, ok := errors.AsType[*domain.TrackerError](err)
	if !ok || te.Kind != domain.ErrTrackerPayload || !strings.Contains(te.Message, "billing/a") || strings.Contains(te.Message, "billing/b") {
		t.Fatalf("Apply() error = %v, want a payload error naming billing/a only", err)
	}
	if len(f.log) != 0 {
		t.Errorf("writes = %v, want none", f.log)
	}

	// A version 1 graph names no repository on any story.
	g1 := newGraph().epic("billing", ApprovalApproved, "a").build()
	if p := preview(t, g1, tr); !slices.Equal(p.MissingRepositories, []string{"billing/a"}) {
		t.Errorf("v1 missing = %v, want billing/a", p.MissingRepositories)
	}
}

// With a default repository, a story that names no repository syncs.
func TestPlanSync_AcceptsStoryWithoutRepositoryUnderADefault(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	tr := withRepositories(f.tracker(), "sortie", "homelab")
	tr.RequiresRepository = func() bool { return false }
	g := v2(newGraph().epic("billing", ApprovalApproved, "a").build())

	if p := preview(t, g, tr); len(p.MissingRepositories) != 0 {
		t.Errorf("missing = %v, want none", p.MissingRepositories)
	}
	apply(t, tr, g)
}

// Without a default, a story that names a listed repository syncs,
// including a repository the workflow lists closed to dispatch.
func TestPlanSync_AcceptsListedRepositoryWithoutDefault(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	// homelab stands for an entry with dispatch: false: plan sync sees
	// every listed name, open or closed.
	tr := requiringRepository(f.tracker(), "sortie", "homelab")
	g := v2(newGraph().epic("billing", ApprovalApproved, "a", "b").build())
	story(&g, "billing/a").Repository = "sortie"
	story(&g, "billing/b").Repository = "homelab"

	if p := preview(t, g, tr); len(p.MissingRepositories) != 0 || len(p.UnlistedRepositories) != 0 {
		t.Errorf("missing = %v, unlisted = %v, want none", p.MissingRepositories, p.UnlistedRepositories)
	}
	apply(t, tr, g)
	for _, i := range f.issues {
		if i.PlanKey == "billing/b" && !slices.Contains(i.Labels, RepositoryLabelPrefix+"homelab") {
			t.Errorf("billing/b labels = %v, want the homelab repository label", i.Labels)
		}
	}
}
