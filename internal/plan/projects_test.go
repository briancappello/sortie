package plan

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// ensuringTracker is a fake tracker whose workspace has the projects in
// exists, and that creates projects on ensure.
type ensuringTracker struct {
	*fakeTracker
	mu     sync.Mutex
	exists map[string]bool
}

func (e *ensuringTracker) InspectPlanProject(_ context.Context, key string) (domain.PlanProject, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return domain.PlanProject{Key: key, Exists: e.exists[key]}, nil
}

func (e *ensuringTracker) EnsurePlanProject(_ context.Context, key, name string) (domain.PlanProject, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	created := !e.exists[key]
	e.exists[key] = true
	return domain.PlanProject{Key: key, Name: name, Exists: true, Created: created}, nil
}

func newEnsuringProjects(t *testing.T) (*Projects, map[string]*fakeTracker, *int) {
	t.Helper()
	def := &ensuringTracker{fakeTracker: newFakeTracker(), exists: map[string]bool{"HULY": true, "BILL": true}}
	built := map[string]*fakeTracker{}
	released := 0
	factory := func(_ context.Context, key string) (Tracker, func(), error) {
		f := newFakeTracker()
		built[key] = f
		return f.tracker(), func() { released++ }, nil
	}
	tr := def.tracker()
	tr.Adapter = def
	return NewProjects("HULY", NewSyncer(tr), factory), built, &released
}

func TestProjectsServeTheConfiguredProjectByDefault(t *testing.T) {
	t.Parallel()

	p, built, _ := newEnsuringProjects(t)
	for _, key := range []string{"", "HULY"} {
		s, err := p.Syncer(context.Background(), key)
		if err != nil || s != p.syncers["HULY"] {
			t.Errorf("Syncer(%q) = %p, %v; want the configured syncer", key, s, err)
		}
	}
	if len(built) != 0 {
		t.Errorf("built %d trackers, want none for the configured project", len(built))
	}
	if !p.IsDefault("") || !p.IsDefault("HULY") || p.IsDefault("BILL") {
		t.Error("IsDefault() is wrong")
	}
}

func TestProjectsBuildEachOtherProjectOnce(t *testing.T) {
	t.Parallel()

	p, built, released := newEnsuringProjects(t)
	first, err := p.Syncer(context.Background(), "BILL")
	if err != nil {
		t.Fatalf("Syncer(BILL) error = %v", err)
	}
	again, _ := p.Syncer(context.Background(), "BILL")
	if first != again || len(built) != 1 || first.Tracker().Adapter != built["BILL"] {
		t.Errorf("Syncer(BILL) twice built %d trackers, want one reused syncer", len(built))
	}
	def, _ := p.Syncer(context.Background(), "")
	if first == def {
		t.Error("BILL shares the configured project's syncer, want its own")
	}
	p.Close()
	if *released != 1 {
		t.Errorf("released = %d, want 1", *released)
	}
}

func TestProjectsRefuseAProjectTheWorkspaceLacks(t *testing.T) {
	t.Parallel()

	p, built, _ := newEnsuringProjects(t)
	if _, err := p.Syncer(context.Background(), "NOPE"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("Syncer(NOPE) error = %v, want ErrProjectNotFound", err)
	}
	if len(built) != 0 {
		t.Errorf("built %d trackers for a missing project, want none", len(built))
	}

	got, err := p.Ensure(context.Background(), "NOPE", "Nope")
	if err != nil || !got.Created {
		t.Fatalf("Ensure(NOPE) = %+v, %v; want created", got, err)
	}
	if _, err := p.Syncer(context.Background(), "NOPE"); err != nil {
		t.Errorf("Syncer(NOPE) after Ensure error = %v", err)
	}
}

func TestProjectsWithoutFactoryServeOnlyTheConfiguredProject(t *testing.T) {
	t.Parallel()

	def := &ensuringTracker{fakeTracker: newFakeTracker(), exists: map[string]bool{"HULY": true}}
	tr := def.tracker()
	tr.Adapter = def
	p := NewProjects("HULY", NewSyncer(tr), nil)

	if _, err := p.Syncer(context.Background(), "BILL"); !errors.Is(err, ErrProjectsUnsupported) {
		t.Errorf("Syncer(BILL) error = %v, want ErrProjectsUnsupported", err)
	}
	if _, err := p.Inspect(context.Background(), "HULY"); !errors.Is(err, ErrProjectsUnsupported) {
		t.Errorf("Inspect() error = %v, want ErrProjectsUnsupported", err)
	}
}

func TestProjectsWithATrackerThatCannotEnsure(t *testing.T) {
	t.Parallel()

	f := newFakeTracker()
	factory := func(context.Context, string) (Tracker, func(), error) { return newFakeTracker().tracker(), nil, nil }
	p := NewProjects("HULY", NewSyncer(f.tracker()), factory)

	if _, err := p.Ensure(context.Background(), "BILL", "Billing"); !errors.Is(err, ErrProjectsUnsupported) {
		t.Errorf("Ensure() error = %v, want ErrProjectsUnsupported", err)
	}
	if _, err := p.Syncer(context.Background(), "BILL"); err != nil {
		t.Errorf("Syncer(BILL) error = %v, want the factory's tracker", err)
	}
}
