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
