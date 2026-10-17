package huly

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tracker/huly/markup"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

const (
	collectionSubIssues = "subIssues"
	spaceSpace          = "core:space:Space"
)

// bodyDigest is the hex SHA-256 of a Markdown body, stored beside the
// issue so a sync can compare bodies without reading markup back.
func bodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// CreateIssue creates an issue the way Huly's own importer does: it takes
// the next number from the project's sequence, ranks the issue after the
// project's last one, stores the description as a collaborative document,
// and creates the issue in its parent's sub-issue collection.
func (a *HulyAdapter) CreateIssue(ctx context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	var out domain.CreatedIssue
	err := trackermetrics.Track(a.recorder(), "create_issue", func() error {
		var err error
		out, err = a.createIssue(ctx, in)
		return err
	})
	return out, err
}

func (a *HulyAdapter) createIssue(ctx context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return domain.CreatedIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: an issue needs a title"}
	}
	if strings.TrimSpace(in.PlanKey) == "" {
		return domain.CreatedIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: a plan issue needs a plan key"}
	}
	taskType, err := a.plan.forKind(in.Kind)
	if err != nil {
		return domain.CreatedIssue{}, err
	}
	if err := a.ensurePlanAttributes(ctx); err != nil {
		return domain.CreatedIssue{}, err
	}

	if existing, err := a.FindIssueByPlanKey(ctx, in.PlanKey); err == nil {
		return domain.CreatedIssue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: plan key %q is already on %s", in.PlanKey, existing.Identifier),
		}
	} else if !isNotFound(err) {
		return domain.CreatedIssue{}, err
	}

	attachedTo := noParent
	parents := []map[string]any{}
	if in.ParentID != "" {
		parent, err := a.projectIssue(ctx, in.ParentID)
		if err != nil {
			return domain.CreatedIssue{}, err
		}
		attachedTo = parent.ID
		parents = parentChain(parent)
	}

	rank, err := a.nextRank(ctx)
	if err != nil {
		return domain.CreatedIssue{}, err
	}
	number, err := a.nextNumber(ctx)
	if err != nil {
		return domain.CreatedIssue{}, err
	}
	identifier := a.project.Identifier + "-" + strconv.Itoa(number)

	id := generateID()
	var description any
	if strings.TrimSpace(in.Body) != "" {
		ref, err := a.rpc.CreateMarkup(ctx, classIssue, id, "description", markup.FromMarkdown(in.Body))
		if err != nil {
			return domain.CreatedIssue{}, err
		}
		description = ref
	}

	tx := a.createTx(classIssue, a.project.ID, id, map[string]any{
		"title":            title,
		"description":      description,
		"assignee":         nil,
		"component":        nil,
		"milestone":        nil,
		"number":           number,
		"status":           a.initialStatus(taskType),
		"priority":         0,
		"rank":             rank,
		"comments":         0,
		"subIssues":        0,
		"startDate":        nil,
		"dueDate":          nil,
		"parents":          parents,
		"remainingTime":    0,
		"estimation":       0,
		"reportedTime":     0,
		"reports":          0,
		"relations":        []any{},
		"childInfo":        []any{},
		"identifier":       identifier,
		"kind":             taskType.ID,
		attrPlanKey:        in.PlanKey,
		attrPlanBodyDigest: bodyDigest(in.Body),
	})
	attach(tx, attachedTo, collectionSubIssues)
	if err := a.rpc.Tx(ctx, tx); err != nil {
		return domain.CreatedIssue{}, err
	}
	return domain.CreatedIssue{ID: id, Identifier: identifier}, nil
}

// projectIssue reads an issue of the configured project, by id or by
// identifier. An issue of another project is not found.
func (a *HulyAdapter) projectIssue(ctx context.Context, issueID string) (hulyIssue, error) {
	issue, err := a.findIssue(ctx, issueID)
	if err != nil {
		return hulyIssue{}, err
	}
	if issue.Space != a.project.ID {
		return hulyIssue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("huly: issue %s is not in project %s", issueID, a.project.Identifier),
		}
	}
	return issue, nil
}

// parentChain is the parents list of a child of parent: the parent first,
// then the parent's own parents.
func parentChain(parent hulyIssue) []map[string]any {
	chain := []map[string]any{{
		"parentId":    parent.ID,
		"parentTitle": parent.Title,
		"space":       parent.Space,
		"identifier":  parent.Identifier,
	}}
	for _, p := range parent.Parents {
		chain = append(chain, map[string]any{
			"parentId":    p.ParentID,
			"parentTitle": p.ParentTitle,
			"space":       p.Space,
			"identifier":  p.Identifier,
		})
	}
	return chain
}

// initialStatus is the project's default issue status when the task type
// allows it, and the type's first status otherwise.
func (a *HulyAdapter) initialStatus(t taskTypeRef) string {
	if a.project.DefaultIssueStatus != "" && slices.Contains(t.Statuses, a.project.DefaultIssueStatus) {
		return a.project.DefaultIssueStatus
	}
	if len(t.Statuses) > 0 {
		return t.Statuses[0]
	}
	return a.project.DefaultIssueStatus
}

// nextNumber increments the project's issue sequence and returns the new
// value. The transactor applies $inc atomically and returns the updated
// project because the transaction sets retrieve, so two writers never
// get the same number.
func (a *HulyAdapter) nextNumber(ctx context.Context) (int, error) {
	tx := a.updateTx(classProject, spaceSpace, a.project.ID, map[string]any{"$inc": map[string]any{"sequence": 1}})
	tx["retrieve"] = true
	raw, err := a.rpc.TxResult(ctx, tx)
	if err != nil {
		return 0, err
	}
	var result struct {
		Object struct {
			Sequence *float64 `json:"sequence"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, payloadError("decode sequence increment", err)
	}
	if result.Object.Sequence == nil || *result.Object.Sequence < 1 {
		return 0, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: the sequence increment returned no number"}
	}
	return int(*result.Object.Sequence), nil
}

// nextRank returns a rank after the project's highest-ranked issue.
func (a *HulyAdapter) nextRank(ctx context.Context) (string, error) {
	raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{"space": idIn([]string{a.project.ID})}, map[string]any{
		"sort":       map[string]any{"rank": -1},
		"limit":      1,
		"projection": map[string]any{"_id": 1, "rank": 1},
	})
	if err != nil {
		return "", err
	}
	issues, err := decodeAll[hulyIssue](raws)
	if err != nil {
		return "", err
	}
	last := ""
	if len(issues) > 0 {
		last = issues[0].Rank
	}
	rank, err := makeRank(last, "")
	if err != nil {
		return "", payloadError("rank after "+last, err)
	}
	return rank, nil
}

func isNotFound(err error) bool {
	te, ok := errors.AsType[*domain.TrackerError](err)
	return ok && te.Kind == domain.ErrTrackerNotFound
}

// UpdateIssue changes the title, body, or kind of a project issue. A
// field that already holds the value is not written; with nothing to
// change, no request is sent beyond the read.
func (a *HulyAdapter) UpdateIssue(ctx context.Context, issueID string, fields domain.IssueFields) error {
	return trackermetrics.Track(a.recorder(), "update_issue", func() error {
		issue, err := a.projectIssue(ctx, issueID)
		if err != nil {
			return err
		}
		ops := map[string]any{}
		if fields.Title != nil {
			title := strings.TrimSpace(*fields.Title)
			if title == "" {
				return &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: an issue needs a title"}
			}
			if title != issue.Title {
				ops["title"] = title
			}
		}
		if fields.Kind != nil {
			taskType, err := a.plan.forKind(*fields.Kind)
			if err != nil {
				return err
			}
			if taskType.ID != issue.Kind {
				ops["kind"] = taskType.ID
			}
		}
		if fields.Body != nil && bodyDigest(*fields.Body) != issue.PlanBodyDigest {
			if err := a.ensurePlanAttributes(ctx); err != nil {
				return err
			}
			serialized := markup.FromMarkdown(*fields.Body)
			if issue.Description != nil && *issue.Description != "" {
				if err := a.rpc.UpdateMarkup(ctx, classIssue, issue.ID, "description", serialized); err != nil {
					return err
				}
			} else {
				ref, err := a.rpc.CreateMarkup(ctx, classIssue, issue.ID, "description", serialized)
				if err != nil {
					return err
				}
				ops["description"] = ref
			}
			ops[attrPlanBodyDigest] = bodyDigest(*fields.Body)
		}
		if len(ops) == 0 {
			return nil
		}
		return a.rpc.Tx(ctx, a.updateTx(classIssue, issue.Space, issue.ID, ops))
	})
}

// SetParent moves a project issue under parentID, or to the top level when
// parentID is empty. It updates attachedTo and the parents chain in one
// transaction filed in the old parent's sub-issue collection, as Huly's
// own parent picker does. Setting the parent the issue has is a no-op.
func (a *HulyAdapter) SetParent(ctx context.Context, issueID, parentID string) error {
	return trackermetrics.Track(a.recorder(), "set_parent", func() error {
		issue, err := a.projectIssue(ctx, issueID)
		if err != nil {
			return err
		}
		current := issue.AttachedTo
		if current == "" {
			current = noParent
		}

		target, chain := noParent, []map[string]any{}
		if parentID != "" {
			parent, err := a.projectIssue(ctx, parentID)
			if err != nil {
				return err
			}
			if parent.ID == issue.ID || slices.ContainsFunc(parent.Parents, func(p parentInfo) bool { return p.ParentID == issue.ID }) {
				return &domain.TrackerError{
					Kind:    domain.ErrTrackerPayload,
					Message: fmt.Sprintf("huly: %s cannot be a parent of itself or of its own ancestor", issue.Identifier),
				}
			}
			target, chain = parent.ID, parentChain(parent)
		}
		if target == current {
			return nil
		}

		tx := a.updateTx(classIssue, issue.Space, issue.ID, map[string]any{"attachedTo": target, "parents": chain})
		attach(tx, current, collectionSubIssues)
		return a.rpc.Tx(ctx, tx)
	})
}

// AddDependency records that blockerID blocks blockedID, in the blocked
// issue's blockedBy list. A relation that exists already is left alone.
func (a *HulyAdapter) AddDependency(ctx context.Context, blockerID, blockedID string) error {
	return trackermetrics.Track(a.recorder(), "add_dependency", func() error {
		blocker, blocked, err := a.dependencyPair(ctx, blockerID, blockedID)
		if err != nil {
			return err
		}
		if slices.Contains(blockerIDs(blocked), blocker.ID) {
			return nil
		}
		return a.rpc.Tx(ctx, a.updateTx(classIssue, blocked.Space, blocked.ID, map[string]any{
			"$push": map[string]any{"blockedBy": map[string]any{"_id": blocker.ID, "_class": classIssue}},
		}))
	})
}

// RemoveDependency removes the relation. A relation that does not exist
// is left alone.
func (a *HulyAdapter) RemoveDependency(ctx context.Context, blockerID, blockedID string) error {
	return trackermetrics.Track(a.recorder(), "remove_dependency", func() error {
		blocker, blocked, err := a.dependencyPair(ctx, blockerID, blockedID)
		if err != nil {
			return err
		}
		if !slices.Contains(blockerIDs(blocked), blocker.ID) {
			return nil
		}
		return a.rpc.Tx(ctx, a.updateTx(classIssue, blocked.Space, blocked.ID, map[string]any{
			"$pull": map[string]any{"blockedBy": map[string]any{"_id": blocker.ID}},
		}))
	})
}

func (a *HulyAdapter) dependencyPair(ctx context.Context, blockerID, blockedID string) (hulyIssue, hulyIssue, error) {
	blocker, err := a.projectIssue(ctx, blockerID)
	if err != nil {
		return hulyIssue{}, hulyIssue{}, err
	}
	blocked, err := a.projectIssue(ctx, blockedID)
	if err != nil {
		return hulyIssue{}, hulyIssue{}, err
	}
	if blocker.ID == blocked.ID {
		return hulyIssue{}, hulyIssue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("huly: %s cannot block itself", blocked.Identifier),
		}
	}
	return blocker, blocked, nil
}
