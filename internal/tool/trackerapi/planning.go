package trackerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tool/toolresult"
)

// planningOperation runs one enabled planning operation. Every issue it
// names is read and checked against the project first, so an operation
// that names an issue outside the project writes nothing.
func (t *TrackerAPITool) planningOperation(ctx context.Context, in toolInput) (json.RawMessage, error) {
	switch registry.WriteCapability(in.Operation) {
	case registry.CapCreateIssue:
		return t.createIssue(ctx, in)
	case registry.CapUpdateIssue:
		return t.updateIssue(ctx, in)
	case registry.CapSetParent:
		return t.setParent(ctx, in)
	default:
		return t.linkDependency(ctx, in)
	}
}

// scoped reads an issue and checks that it is in the project. The second
// result, when non-nil, is the failure envelope to return.
func (t *TrackerAPITool) scoped(ctx context.Context, issueID string) (domain.Issue, json.RawMessage, error) {
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		out, ferr := mapTrackerError(err)
		return domain.Issue{}, out, ferr
	}
	if !t.isInProject(issue.Identifier) {
		out, ferr := toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
		return domain.Issue{}, out, ferr
	}
	return issue, nil, nil
}

// issueResult returns the issue in the fetch_issue shape, with extra
// fields added.
func (t *TrackerAPITool) issueResult(ctx context.Context, issueID string, extra map[string]any) (json.RawMessage, error) {
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}
	m := issue.ToTemplateMap()
	maps.Copy(m, extra)
	return toolresult.Success(m)
}

func parseKind(raw *string) (domain.IssueKind, bool) {
	if raw == nil {
		return "", false
	}
	k := domain.IssueKind(strings.TrimSpace(*raw))
	return k, k.Valid()
}

// parentArg reads parent_id: present reports whether the field was sent,
// and id is empty for null.
func parentArg(raw json.RawMessage) (id string, present bool, err error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if string(raw) == "null" {
		return "", true, nil
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", true, fmt.Errorf("parent_id must be a string or null")
	}
	return id, true, nil
}

func (t *TrackerAPITool) createIssue(ctx context.Context, in toolInput) (json.RawMessage, error) {
	if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
		return toolresult.Failure("invalid_input", "title is required for create_issue")
	}
	kind, ok := parseKind(in.Kind)
	if !ok {
		return toolresult.Failure("invalid_input", "kind must be epic or story for create_issue")
	}
	if strings.TrimSpace(in.PlanKey) == "" {
		return toolresult.Failure("invalid_input", "plan_key is required for create_issue")
	}
	parentID, _, err := parentArg(in.ParentID)
	if err != nil {
		return toolresult.Failure("invalid_input", err.Error())
	}
	if parentID != "" {
		parent, fail, ferr := t.scoped(ctx, parentID)
		if fail != nil || ferr != nil {
			return fail, ferr
		}
		parentID = parent.ID
	}
	body := ""
	if in.Body != nil {
		body = *in.Body
	}
	created, err := t.adapter.(domain.IssueCreator).CreateIssue(ctx, domain.NewIssue{
		Title: *in.Title, Body: body, Kind: kind, PlanKey: in.PlanKey, ParentID: parentID,
	})
	if err != nil {
		return mapTrackerError(err)
	}
	return t.issueResult(ctx, created.ID, map[string]any{"plan_key": in.PlanKey, "kind": string(kind)})
}

func (t *TrackerAPITool) updateIssue(ctx context.Context, in toolInput) (json.RawMessage, error) {
	if in.IssueID == "" {
		return toolresult.Failure("invalid_input", "issue_id is required for update_issue")
	}
	if in.Title == nil && in.Body == nil && in.Kind == nil {
		return toolresult.Failure("invalid_input", "update_issue needs at least one of title, body, kind")
	}
	fields := domain.IssueFields{Title: in.Title, Body: in.Body}
	if in.Kind != nil {
		kind, ok := parseKind(in.Kind)
		if !ok {
			return toolresult.Failure("invalid_input", "kind must be epic or story")
		}
		fields.Kind = &kind
	}
	issue, fail, ferr := t.scoped(ctx, in.IssueID)
	if fail != nil || ferr != nil {
		return fail, ferr
	}
	if err := t.adapter.(domain.IssueUpdater).UpdateIssue(ctx, issue.ID, fields); err != nil {
		return mapTrackerError(err)
	}
	return t.issueResult(ctx, issue.ID, nil)
}

func (t *TrackerAPITool) setParent(ctx context.Context, in toolInput) (json.RawMessage, error) {
	if in.IssueID == "" {
		return toolresult.Failure("invalid_input", "issue_id is required for set_parent")
	}
	parentID, present, err := parentArg(in.ParentID)
	if err != nil {
		return toolresult.Failure("invalid_input", err.Error())
	}
	if !present {
		return toolresult.Failure("invalid_input", "parent_id is required for set_parent; send null to remove the parent")
	}
	issue, fail, ferr := t.scoped(ctx, in.IssueID)
	if fail != nil || ferr != nil {
		return fail, ferr
	}
	if parentID != "" {
		parent, fail, ferr := t.scoped(ctx, parentID)
		if fail != nil || ferr != nil {
			return fail, ferr
		}
		parentID = parent.ID
	}
	if err := t.adapter.(domain.IssueParenter).SetParent(ctx, issue.ID, parentID); err != nil {
		return mapTrackerError(err)
	}
	return t.issueResult(ctx, issue.ID, nil)
}

func (t *TrackerAPITool) linkDependency(ctx context.Context, in toolInput) (json.RawMessage, error) {
	if in.BlockerID == "" || in.BlockedID == "" {
		return toolresult.Failure("invalid_input", "blocker_id and blocked_id are required for link_dependency")
	}
	if in.Action != "add" && in.Action != "remove" {
		return toolresult.Failure("invalid_input", "action must be add or remove for link_dependency")
	}
	blocker, fail, ferr := t.scoped(ctx, in.BlockerID)
	if fail != nil || ferr != nil {
		return fail, ferr
	}
	blocked, fail, ferr := t.scoped(ctx, in.BlockedID)
	if fail != nil || ferr != nil {
		return fail, ferr
	}
	linker := t.adapter.(domain.DependencyLinker)
	var err error
	if in.Action == "add" {
		err = linker.AddDependency(ctx, blocker.ID, blocked.ID)
	} else {
		err = linker.RemoveDependency(ctx, blocker.ID, blocked.ID)
	}
	if err != nil {
		return mapTrackerError(err)
	}
	return t.issueResult(ctx, blocked.ID, nil)
}
