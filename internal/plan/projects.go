package plan

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
)

// ErrProjectNotFound is returned for a project the tracker workspace does
// not have.
var ErrProjectNotFound = errors.New("plan: project not found")

// ErrProjectsUnsupported is returned when the tracker cannot inspect or
// create projects, or when only the configured project is served.
var ErrProjectsUnsupported = errors.New("plan: this tracker serves plan sync for its configured project only")

// TrackerFactory builds the tracker for one project of the configured
// workspace, and a function that releases it.
type TrackerFactory func(ctx context.Context, project string) (Tracker, func(), error)

// Projects serves plan sync for several projects of one tracker
// workspace: the configured project, whose tracker the orchestrator
// built, and others built on first use. Each project has its own
// [Syncer], so previews and the apply lock are per project. Safe for
// concurrent use.
type Projects struct {
	defaultKey string
	factory    TrackerFactory
	ensurer    domain.PlanProjectEnsurer

	mu      sync.Mutex
	syncers map[string]*Syncer
	closers []func()
}

// NewProjects serves the configured project defaultKey through def.
// factory builds the trackers of other projects; nil serves only the
// configured project.
func NewProjects(defaultKey string, def *Syncer, factory TrackerFactory) *Projects {
	p := &Projects{
		defaultKey: defaultKey,
		factory:    factory,
		syncers:    map[string]*Syncer{defaultKey: def},
	}
	if e, ok := def.Tracker().Adapter.(domain.PlanProjectEnsurer); ok && factory != nil {
		p.ensurer = e
	}
	return p
}

// Default returns the configured project's key.
func (p *Projects) Default() string { return p.defaultKey }

// IsDefault reports whether key, or the empty key, names the configured
// project.
func (p *Projects) IsDefault(key string) bool { return key == "" || key == p.defaultKey }

// Syncer returns the syncer of project key; the empty key is the
// configured project. A project the workspace lacks is
// [ErrProjectNotFound].
func (p *Projects) Syncer(ctx context.Context, key string) (*Syncer, error) {
	if key == "" {
		key = p.defaultKey
	}
	p.mu.Lock()
	s, ok := p.syncers[key]
	p.mu.Unlock()
	if ok {
		return s, nil
	}
	if p.factory == nil {
		return nil, ErrProjectsUnsupported
	}
	if p.ensurer != nil {
		got, err := p.ensurer.InspectPlanProject(ctx, key)
		if err != nil {
			return nil, err
		}
		if !got.Exists {
			return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, key)
		}
	}
	t, release, err := p.factory(ctx, key)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.syncers[key]; ok { // built meanwhile by another caller
		if release != nil {
			release()
		}
		return s, nil
	}
	s = NewSyncer(t)
	p.syncers[key] = s
	if release != nil {
		p.closers = append(p.closers, release)
	}
	return s, nil
}

// Inspect reports project key as plan sync needs it.
func (p *Projects) Inspect(ctx context.Context, key string) (domain.PlanProject, error) {
	if p.ensurer == nil {
		return domain.PlanProject{}, ErrProjectsUnsupported
	}
	return p.ensurer.InspectPlanProject(ctx, key)
}

// Ensure creates project key named name when the workspace lacks it, then
// inspects it.
func (p *Projects) Ensure(ctx context.Context, key, name string) (domain.PlanProject, error) {
	if p.ensurer == nil {
		return domain.PlanProject{}, ErrProjectsUnsupported
	}
	return p.ensurer.EnsurePlanProject(ctx, key, name)
}

// Close releases the trackers built for other projects.
func (p *Projects) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.closers {
		c()
	}
	p.closers = nil
}
