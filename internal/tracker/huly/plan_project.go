package huly

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

var _ domain.PlanProjectEnsurer = (*HulyAdapter)(nil)

// spaceSpaces is the space Huly keeps project documents in.
const spaceSpaces = "core:space:Space"

// projectKeyPattern is the identifier Huly's project form accepts: a
// letter, then up to four letters or digits, upper case.
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,4}$`)

// InspectPlanProject reports the project with key in the workspace and
// what keeps plan writes from working in it: a task type the configured
// epic or story type names that the project's space type lacks, or a
// configured state its workflow lacks.
func (a *HulyAdapter) InspectPlanProject(ctx context.Context, key string) (domain.PlanProject, error) {
	if !projectKeyPattern.MatchString(key) {
		return domain.PlanProject{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: project key %q must be a letter then up to four letters or digits, upper case", key),
		}
	}
	project, found, err := a.findProject(ctx, key)
	if err != nil || !found {
		return domain.PlanProject{Key: key}, err
	}
	out := domain.PlanProject{
		Key: key, Name: project.Name, Exists: true,
		URL: a.issueURLBase + url.PathEscape(project.ID) + "/issues",
	}
	problems, err := a.planProblems(ctx, project)
	if err != nil {
		return domain.PlanProject{}, err
	}
	out.Problems = problems
	return out, nil
}

// EnsurePlanProject creates a classic project with key and name, of the
// same space type as the configured project, when the workspace has no
// project with key; then it inspects the project. A project that exists
// is never changed.
func (a *HulyAdapter) EnsurePlanProject(ctx context.Context, key, name string) (domain.PlanProject, error) {
	a.ensureMu.Lock()
	defer a.ensureMu.Unlock()

	got, err := a.InspectPlanProject(ctx, key)
	if err != nil || got.Exists {
		return got, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = key
	}
	members := []string{}
	if a.accountUUID != "" {
		members = append(members, a.accountUUID)
	}
	tx := a.createTx(classProject, spaceSpaces, generateID(), map[string]any{
		"name": name, "description": "", "private": false, "archived": false, "autoJoin": true,
		"members": members, "owners": members, "identifier": key, "sequence": 0,
		"defaultIssueStatus": a.project.DefaultIssueStatus, "defaultTimeReportDay": "PreviousWorkDay",
		"type": a.project.Type,
	})
	if err := a.rpc.Tx(ctx, tx); err != nil {
		return domain.PlanProject{}, err
	}
	a.log.Info("huly project created for plan sync", slog.String("project", key))

	got, err = a.InspectPlanProject(ctx, key)
	if err != nil {
		return domain.PlanProject{}, err
	}
	if !got.Exists {
		return domain.PlanProject{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("huly: created project %s but cannot read it back", key),
		}
	}
	got.Created = true
	return got, nil
}

func (a *HulyAdapter) findProject(ctx context.Context, key string) (hulyProject, bool, error) {
	raws, err := a.rpc.FindAll(ctx, classProject, map[string]any{"identifier": idIn([]string{key})}, map[string]any{"limit": 1})
	if err != nil {
		return hulyProject{}, false, err
	}
	projects, err := decodeAll[hulyProject](raws)
	if err != nil || len(projects) == 0 {
		return hulyProject{}, false, err
	}
	return projects[0], true, nil
}

// planProblems lists what keeps plan writes from working in project.
func (a *HulyAdapter) planProblems(ctx context.Context, project hulyProject) ([]string, error) {
	problems := []string{}

	raws, err := a.rpc.FindAll(ctx, classProjectType, map[string]any{"_id": idIn([]string{project.Type})}, map[string]any{"limit": 1})
	if err != nil {
		return nil, err
	}
	types, err := decodeAll[hulyProjectType](raws)
	if err != nil {
		return nil, err
	}
	if len(types) == 0 {
		return append(problems, fmt.Sprintf("its space type %q is missing from the workspace", project.Type)), nil
	}
	ptype := types[0]
	typeName := cmp.Or(ptype.Name, ptype.ID)

	shared, err := a.projectsOfType(ctx, ptype.ID)
	if err != nil {
		return nil, err
	}
	where := fmt.Sprintf("in Huly Settings, Spaces, space type %q", typeName)
	if shared > 1 {
		where += fmt.Sprintf(" (a change there applies to all %d projects of that type)", shared)
	}

	epicName, storyName := a.planNames[0], a.planNames[1]
	if epicName == "" || storyName == "" {
		problems = append(problems, "Sortie's workflow sets no huly.epic_type and huly.story_type, so it cannot write plan issues")
	} else {
		taskRaws, err := a.rpc.FindAll(ctx, classTaskType, map[string]any{"parent": idIn([]string{ptype.ID})}, nil)
		if err != nil {
			return nil, err
		}
		tasks, err := decodeAll[hulyTaskType](taskRaws)
		if err != nil {
			return nil, err
		}
		for _, want := range []string{epicName, storyName} {
			if !slices.ContainsFunc(tasks, func(t hulyTaskType) bool { return strings.EqualFold(strings.TrimSpace(t.Name), want) }) {
				problems = append(problems, fmt.Sprintf("it has no task type %q; add it under Task types %s", want, where))
			}
		}
	}

	ids := make([]string, 0, len(ptype.Statuses))
	for _, s := range ptype.Statuses {
		ids = append(ids, s.ID)
	}
	statuses, err := a.lookupStatuses(ctx, ids)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, s := range statuses {
		have[strings.ToLower(strings.TrimSpace(s.Name))] = true
	}
	var missing []string
	for _, name := range a.requiredStates {
		if !have[strings.ToLower(strings.TrimSpace(name))] && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("its workflow lacks the state(s) %s that Sortie's workflow names; add them %s",
			strings.Join(missing, ", "), where))
	}
	return problems, nil
}

func (a *HulyAdapter) projectsOfType(ctx context.Context, typeID string) (int, error) {
	raws, err := a.rpc.FindAll(ctx, classProject, map[string]any{"type": typeID}, nil)
	if err != nil {
		return 0, err
	}
	return len(raws), nil
}
