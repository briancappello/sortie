package huly

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// taskTypeRef is a project task type the plan kinds map to.
type taskTypeRef struct {
	ID       string
	Name     string
	Statuses []string
}

// planTypes maps the planning kinds to the project's task types. Both
// are zero when huly.epic_type and huly.story_type are not configured.
type planTypes struct {
	epic  taskTypeRef
	story taskTypeRef
}

func (p planTypes) configured() bool {
	return p.epic.ID != "" && p.story.ID != ""
}

// forKind returns the task type a planning kind maps to.
func (p planTypes) forKind(kind domain.IssueKind) (taskTypeRef, error) {
	if !p.configured() {
		return taskTypeRef{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "huly: set huly.epic_type and huly.story_type to write plan issues",
		}
	}
	switch kind {
	case domain.IssueKindEpic:
		return p.epic, nil
	case domain.IssueKindStory:
		return p.story, nil
	default:
		return taskTypeRef{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: unknown issue kind %q", kind),
		}
	}
}

// kindOf returns the planning kind of a task type id, empty when the type
// is neither the epic nor the story type.
func (p planTypes) kindOf(taskTypeID string) domain.IssueKind {
	switch {
	case taskTypeID == "":
		return ""
	case taskTypeID == p.epic.ID:
		return domain.IssueKindEpic
	case taskTypeID == p.story.ID:
		return domain.IssueKindStory
	default:
		return ""
	}
}

type hulyTaskType struct {
	ID       string   `json:"_id"`
	Name     string   `json:"name"`
	Statuses []string `json:"statuses"`
}

// resolvePlanTypes resolves the configured epic and story task type names
// against the task types of the project's type, compared without regard
// to letter case. Both names empty leaves the plan kinds unmapped; one
// name without the other, or a name the project lacks, is an error that
// lists the project's task types.
func (a *HulyAdapter) resolvePlanTypes(ctx context.Context, epicName, storyName string) error {
	epicName, storyName = strings.TrimSpace(epicName), strings.TrimSpace(storyName)
	if epicName == "" && storyName == "" {
		return nil
	}
	if epicName == "" || storyName == "" {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "huly: set both huly.epic_type and huly.story_type, or neither",
		}
	}
	if strings.EqualFold(epicName, storyName) {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "huly: huly.epic_type and huly.story_type must name different task types",
		}
	}

	raws, err := a.rpc.FindAll(ctx, classTaskType, map[string]any{"parent": idIn([]string{a.project.Type})}, nil)
	if err != nil {
		return err
	}
	types, err := decodeAll[hulyTaskType](raws)
	if err != nil {
		return err
	}

	find := func(name string) (taskTypeRef, bool) {
		for _, t := range types {
			if strings.EqualFold(strings.TrimSpace(t.Name), name) {
				return taskTypeRef{ID: t.ID, Name: t.Name, Statuses: t.Statuses}, true
			}
		}
		return taskTypeRef{}, false
	}

	epic, okEpic := find(epicName)
	story, okStory := find(storyName)
	if !okEpic || !okStory {
		var missing []string
		if !okEpic {
			missing = append(missing, fmt.Sprintf("%q (huly.epic_type)", epicName))
		}
		if !okStory {
			missing = append(missing, fmt.Sprintf("%q (huly.story_type)", storyName))
		}
		names := make([]string, 0, len(types))
		for _, t := range types {
			names = append(names, t.Name)
		}
		slices.Sort(names)
		return &domain.TrackerError{
			Kind: domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: task type(s) %s not in project %s; its task types are %s",
				strings.Join(missing, ", "), a.project.Identifier, strings.Join(names, ", ")),
		}
	}

	a.plan = planTypes{epic: epic, story: story}
	return nil
}
