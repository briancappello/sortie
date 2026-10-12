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
