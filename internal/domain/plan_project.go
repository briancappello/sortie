package domain

import "context"

// PlanProject describes a tracker project as plan sync needs it: whether
// it exists, and what keeps plan writes from working in it.
type PlanProject struct {
	// Key is the project's identifier, the prefix of its issue keys.
	Key string

	// Name is the project's display name, empty when it does not exist.
	Name string

	// URL is the project's page in the tracker, empty when unknown.
	URL string

	// Exists reports whether the tracker has the project.
	Exists bool

	// Created reports whether this call created the project.
	Created bool

	// Problems lists what keeps plan writes from working in the
	// project, each phrased for an operator with the fix it needs.
	// Empty when the project is ready.
	Problems []string
}

// Ready reports whether the project exists and has no problems.
func (p PlanProject) Ready() bool {
	return p.Exists && len(p.Problems) == 0
}

// PlanProjectEnsurer is the optional capability to inspect and create
// projects for plan sync in the configured tracker workspace.
type PlanProjectEnsurer interface {
	// InspectPlanProject reports the project with key and what keeps
	// plan writes from working in it. A missing project is not an
	// error: it reports Exists false.
	InspectPlanProject(ctx context.Context, key string) (PlanProject, error)

	// EnsurePlanProject creates the project with key and name when it
	// does not exist, then inspects it. It never changes a project
	// that exists, nor anything the project shares with others.
	EnsurePlanProject(ctx context.Context, key, name string) (PlanProject, error)
}
