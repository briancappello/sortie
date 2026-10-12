package huly

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// The custom attributes plan writes add to tracker:class:Issue. Huly's
// own settings page creates class attributes the same way, so they need
// no Huly change; the names mark them as Sortie's.
const (
	attrPlanKey        = "sortiePlanKey"
	attrPlanBodyDigest = "sortiePlanBodyDigest"

	classAttribute  = "core:class:Attribute"
	classTypeString = "core:class:TypeString"
	spaceModel      = "core:space:Model"
)

// planAttributes describes each attribute ensurePlanAttributes creates.
var planAttributes = []struct {
	name   string
	label  string
	hidden bool
}{
	{attrPlanKey, "Sortie plan key", false},
	{attrPlanBodyDigest, "Sortie plan body digest", true},
}

type hulyAttribute struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
}

// ensurePlanAttributes creates the plan attributes on tracker:class:Issue
// when the workspace lacks them. It runs before every plan write and
// reads the model only until both attributes are known to exist.
func (a *HulyAdapter) ensurePlanAttributes(ctx context.Context) error {
	a.attrMu.Lock()
	defer a.attrMu.Unlock()
	if a.attrsReady {
		return nil
	}

	names := make([]string, 0, len(planAttributes))
	for _, attr := range planAttributes {
		names = append(names, attr.name)
	}
	raws, err := a.rpc.FindAll(ctx, classAttribute, map[string]any{
		"attributeOf": idIn([]string{classIssue}),
		"name":        idIn(names),
	}, nil)
	if err != nil {
		return err
	}
	existing, err := decodeAll[hulyAttribute](raws)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, e := range existing {
		have[e.Name] = true
	}

	for _, attr := range planAttributes {
		if have[attr.name] {
			continue
		}
		tx := a.createTx(classAttribute, spaceModel, generateID(), map[string]any{
			"attributeOf":    classIssue,
			"name":           attr.name,
			"label":          "embedded:embedded:" + attr.label,
			"isCustom":       true,
			"readonly":       true,
			"automationOnly": true,
			"hidden":         attr.hidden,
			"type":           map[string]any{"_class": classTypeString, "label": "core:string:String"},
		})
		if err := a.rpc.Tx(ctx, tx); err != nil {
			return err
		}
		a.log.Info("created Huly issue attribute for plan writes", slog.String("attribute", attr.name))
	}
	a.attrsReady = true
	return nil
}

// FindIssueByPlanKey returns the project's issue that carries planKey.
func (a *HulyAdapter) FindIssueByPlanKey(ctx context.Context, planKey string) (domain.PlanIssue, error) {
	var out domain.PlanIssue
	err := trackermetrics.Track(a.recorder(), "find_by_plan_key", func() error {
		raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{
			"space":     idIn([]string{a.project.ID}),
			attrPlanKey: idIn([]string{planKey}),
		}, map[string]any{"sort": map[string]any{"createdOn": 1}})
		if err != nil {
			return err
		}
		issues, err := decodeAll[hulyIssue](raws)
		if err != nil {
			return err
		}
		if len(issues) == 0 {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("huly: no issue in project %s carries plan key %q", a.project.Identifier, planKey),
			}
		}
		if len(issues) > 1 {
			a.log.Warn("several Huly issues carry one plan key; using the oldest",
				slog.String("plan_key", planKey), slog.Int("issues", len(issues)))
		}
		planIssues, err := a.toPlanIssues(ctx, issues[:1])
		if err != nil {
			return err
		}
		out = planIssues[0]
		return nil
	})
	return out, err
}

// ListPlanIssues returns the project's plan-keyed issues, and the others
// too when includeUnkeyed is true.
func (a *HulyAdapter) ListPlanIssues(ctx context.Context, includeUnkeyed bool) ([]domain.PlanIssue, error) {
	var out []domain.PlanIssue
	err := trackermetrics.Track(a.recorder(), "list_plan_issues", func() error {
		raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{
			"space": idIn([]string{a.project.ID}),
		}, map[string]any{"sort": map[string]any{"createdOn": 1}})
		if err != nil {
			return err
		}
		issues, err := decodeAll[hulyIssue](raws)
		if err != nil {
			return err
		}
		kept := issues[:0]
		for _, i := range issues {
			if includeUnkeyed || i.PlanKey != "" {
				kept = append(kept, i)
			}
		}
		out, err = a.toPlanIssues(ctx, kept)
		return err
	})
	return out, err
}

func (a *HulyAdapter) toPlanIssues(ctx context.Context, issues []hulyIssue) ([]domain.PlanIssue, error) {
	statusIDs := make([]string, 0, len(issues))
	for _, i := range issues {
		statusIDs = append(statusIDs, i.Status)
	}
	statuses, err := a.lookupStatuses(ctx, dedupe(statusIDs))
	if err != nil {
		return nil, err
	}

	out := make([]domain.PlanIssue, 0, len(issues))
	for _, i := range issues {
		p := domain.PlanIssue{
			ID:         i.ID,
			Identifier: i.Identifier,
			PlanKey:    i.PlanKey,
			Kind:       a.plan.kindOf(i.Kind),
			Title:      i.Title,
			BodyDigest: i.PlanBodyDigest,
			State:      statuses[i.Status].Name,
			URL:        a.issueURLBase + i.Identifier,
			BlockedBy:  blockerIDs(i),
		}
		if i.AttachedTo != noParent {
			p.ParentID = i.AttachedTo
		}
		out = append(out, p)
	}
	return out, nil
}

// blockerIDs returns the ids of the issues that block i. Non-nil.
func blockerIDs(i hulyIssue) []string {
	ids := make([]string, 0, len(i.BlockedBy))
	for _, b := range i.BlockedBy {
		if b.Class == "" || b.Class == classIssue {
			ids = append(ids, b.ID)
		}
	}
	return ids
}
