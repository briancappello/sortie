// Package trackerapi implements [domain.AgentTool] for issue tracker
// operations. It delegates to a [domain.TrackerAdapter] and enforces
// project-level scoping so agents can only query or mutate issues
// within the configured project.
package trackerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tool/toolresult"
)

// Compile-time interface check.
var _ domain.AgentTool = (*TrackerAPITool)(nil)

// TrackerAPITool implements [domain.AgentTool] by delegating to a
// [domain.TrackerAdapter]. Scoped to a single tracker project at
// construction time. Supports fetch_issue, fetch_comments,
// search_issues, and transition_issue operations, plus a planning
// operation for each write capability the adapter declares.
type TrackerAPITool struct {
	adapter domain.TrackerAdapter
	project string
	writes  []registry.WriteCapability
}

// Option configures a [TrackerAPITool].
type Option func(*TrackerAPITool)

// WithWriteCapabilities enables the planning operation of each given
// capability that the adapter also implements: create_issue,
// update_issue, set_parent, and link_dependency. Pass the capabilities
// the adapter's kind declared at registration.
func WithWriteCapabilities(caps []registry.WriteCapability) Option {
	return func(t *TrackerAPITool) {
		for _, c := range registry.AllWriteCapabilities() {
			if slices.Contains(caps, c) && implements(t.adapter, c) {
				t.writes = append(t.writes, c)
			}
		}
	}
}

func implements(adapter domain.TrackerAdapter, c registry.WriteCapability) bool {
	var ok bool
	switch c {
	case registry.CapCreateIssue:
		_, ok = adapter.(domain.IssueCreator)
	case registry.CapUpdateIssue:
		_, ok = adapter.(domain.IssueUpdater)
	case registry.CapSetParent:
		_, ok = adapter.(domain.IssueParenter)
	case registry.CapApprovalFields:
		// Never offered: an agent must not record who approved a story.
		return false
	case registry.CapLinkDependency:
		_, ok = adapter.(domain.DependencyLinker)
	}
	return ok
}

// New creates a [TrackerAPITool] scoped to the given tracker adapter
// and project. Panics if adapter is nil (programming error). The
// project parameter should be non-empty; callers should skip
// construction when tracker.project is unconfigured.
func New(adapter domain.TrackerAdapter, project string, opts ...Option) *TrackerAPITool {
	if adapter == nil {
		panic("trackerapi.New: adapter must not be nil")
	}
	t := &TrackerAPITool{
		adapter: adapter,
		project: project,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns "tracker_api".
func (t *TrackerAPITool) Name() string { return "tracker_api" }

// Description returns a summary of the tool's capabilities.
func (t *TrackerAPITool) Description() string {
	d := "Query or modify issues in the configured issue tracker. " +
		"Supports fetch_issue, fetch_comments, search_issues (active states only), " +
		"and transition_issue operations."
	if len(t.writes) == 0 {
		return d
	}
	ops := make([]string, 0, len(t.writes))
	for _, c := range t.writes {
		ops = append(ops, string(c))
	}
	return d + " Planning operations: " + strings.Join(ops, ", ") +
		". create_issue takes title, body (Markdown), kind (epic or story), plan_key, and an optional parent_id; " +
		"update_issue takes issue_id and any of title, body, kind; set_parent takes issue_id and parent_id (null removes the parent); " +
		"link_dependency takes blocker_id, blocked_id, and action (add or remove)."
}

// hasWrite reports whether the planning operation of c is enabled.
func (t *TrackerAPITool) hasWrite(c registry.WriteCapability) bool {
	return slices.Contains(t.writes, c)
}

// planningProperties are the input fields of the planning operations.
const planningProperties = `,
    "title": {"type": "string", "description": "Issue title (create_issue, update_issue)."},
    "body": {"type": "string", "description": "Issue body in Markdown (create_issue, update_issue)."},
    "kind": {"type": "string", "enum": ["epic", "story"], "description": "Planning kind (create_issue, update_issue)."},
    "plan_key": {"type": "string", "description": "Stable plan key of the new issue (create_issue)."},
    "parent_id": {"type": ["string", "null"], "description": "Parent issue ID (create_issue, set_parent); null removes the parent in set_parent."},
    "blocker_id": {"type": "string", "description": "The blocking issue (link_dependency)."},
    "blocked_id": {"type": "string", "description": "The blocked issue (link_dependency)."},
    "action": {"type": "string", "enum": ["add", "remove"], "description": "Add or remove the relation (link_dependency)."}`

var inputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation": {
      "type": "string",
      "enum": ["fetch_issue", "fetch_comments", "search_issues", "transition_issue"],
      "description": "The tracker operation to perform. search_issues returns active-state issues only."
    },
    "issue_id": {
      "type": "string",
      "description": "The tracker issue ID (required for fetch_issue, fetch_comments, transition_issue)."
    },
    "target_state": {
      "type": "string",
      "description": "The target state for transition_issue."
    }
  },
  "required": ["operation"],
  "additionalProperties": false
}`)

// InputSchema returns the JSON Schema describing the expected input.
// The returned slice is a defensive copy; callers may modify it
// without affecting other consumers. Planning operations and their
// fields appear only when enabled.
func (t *TrackerAPITool) InputSchema() json.RawMessage {
	if len(t.writes) == 0 {
		out := make(json.RawMessage, len(inputSchema))
		copy(out, inputSchema)
		return out
	}
	schema := string(inputSchema)
	quoted := []string{`"fetch_issue"`, `"fetch_comments"`, `"search_issues"`, `"transition_issue"`}
	for _, c := range t.writes {
		quoted = append(quoted, fmt.Sprintf("%q", string(c)))
	}
	ops := strings.Join(quoted, ", ")
	schema = strings.Replace(schema, `"fetch_issue", "fetch_comments", "search_issues", "transition_issue"`, ops, 1)
	schema = strings.Replace(schema, `"description": "The target state for transition_issue."
    }`, `"description": "The target state for transition_issue."
    }`+planningProperties, 1)
	return json.RawMessage(schema)
}

type toolInput struct {
	Operation   string `json:"operation"`
	IssueID     string `json:"issue_id,omitempty"`
	TargetState string `json:"target_state,omitempty"`

	Title     *string         `json:"title,omitempty"`
	Body      *string         `json:"body,omitempty"`
	Kind      *string         `json:"kind,omitempty"`
	PlanKey   string          `json:"plan_key,omitempty"`
	ParentID  json.RawMessage `json:"parent_id,omitempty"`
	BlockerID string          `json:"blocker_id,omitempty"`
	BlockedID string          `json:"blocked_id,omitempty"`
	Action    string          `json:"action,omitempty"`
}

// Execute runs the requested tracker operation and returns a
// structured JSON result. Domain-level errors are encoded in the
// JSON response as {"success": false, "error": {...}}. The Go error
// return is reserved for truly unexpected internal failures.
func (t *TrackerAPITool) Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var in toolInput
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolresult.Failure("invalid_input", fmt.Sprintf("failed to parse input: %s", err))
	}
	if dec.More() {
		return toolresult.Failure("invalid_input", "unexpected trailing content after JSON object")
	}

	if in.Operation == "" {
		return toolresult.Failure("invalid_input", "operation is required")
	}

	switch in.Operation {
	case "fetch_issue":
		if in.IssueID == "" {
			return toolresult.Failure("invalid_input", "issue_id is required for fetch_issue")
		}
		return t.fetchIssue(ctx, in.IssueID)

	case "fetch_comments":
		if in.IssueID == "" {
			return toolresult.Failure("invalid_input", "issue_id is required for fetch_comments")
		}
		return t.fetchComments(ctx, in.IssueID)

	case "search_issues":
		return t.searchIssues(ctx)

	case "transition_issue":
		if in.IssueID == "" {
			return toolresult.Failure("invalid_input", "issue_id is required for transition_issue")
		}
		if in.TargetState == "" {
			return toolresult.Failure("invalid_input", "target_state is required for transition_issue")
		}
		return t.transitionIssue(ctx, in.IssueID, in.TargetState)

	case string(registry.CapCreateIssue), string(registry.CapUpdateIssue), string(registry.CapSetParent), string(registry.CapLinkDependency):
		if !t.hasWrite(registry.WriteCapability(in.Operation)) {
			return toolresult.Failure("unsupported_operation",
				fmt.Sprintf("the configured tracker does not support %s", in.Operation))
		}
		return t.planningOperation(ctx, in)

	default:
		return toolresult.Failure("unsupported_operation", fmt.Sprintf("unknown operation: %s", in.Operation))
	}
}

func (t *TrackerAPITool) fetchIssue(ctx context.Context, issueID string) (json.RawMessage, error) {
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	if !t.isInProject(issue.Identifier) {
		return toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
	}

	return toolresult.Success(issue.ToTemplateMap())
}

func (t *TrackerAPITool) fetchComments(ctx context.Context, issueID string) (json.RawMessage, error) {
	// Fetch issue first for project scope validation (defense-in-depth).
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	if !t.isInProject(issue.Identifier) {
		return toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
	}

	comments, err := t.adapter.FetchIssueComments(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	return toolresult.Success(domain.CommentTemplateMaps(comments))
}

func (t *TrackerAPITool) searchIssues(ctx context.Context) (json.RawMessage, error) {
	issues, err := t.adapter.FetchCandidateIssues(ctx)
	if err != nil {
		return mapTrackerError(err)
	}

	issueMaps := make([]map[string]any, len(issues))
	for i := range issues {
		issueMaps[i] = issues[i].ToTemplateMap()
	}

	return toolresult.Success(issueMaps)
}

func (t *TrackerAPITool) transitionIssue(ctx context.Context, issueID, targetState string) (json.RawMessage, error) {
	// Fetch issue first for project scope validation (defense-in-depth).
	issue, err := t.adapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		return mapTrackerError(err)
	}

	if !t.isInProject(issue.Identifier) {
		return toolresult.Failure("project_scope_violation",
			fmt.Sprintf("issue %s is not in project %s", issue.Identifier, t.project))
	}

	if err := t.adapter.TransitionIssue(ctx, issueID, targetState); err != nil {
		return mapTrackerError(err)
	}

	return toolresult.Success(map[string]any{"transitioned": true})
}

// isInProject is a defense-in-depth check using identifier prefix.
// It is NOT the primary access control; the TrackerAdapter's own
// project scoping (JQL filter, API endpoint) is the primary control.
// This check catches edge cases where an agent passes an issue ID
// from another project that the API key happens to have access to.
//
// Behavior by tracker kind:
//   - Jira/Linear (PROJ-123 format): extracts prefix, compares with project
//   - File adapter (arbitrary format): identifiers without "-" pass (trusted)
//   - GitHub (future, numeric IDs): no "-", passes (adapter is already scoped)
//
// When project is empty, all identifiers pass (no scoping configured).
func (t *TrackerAPITool) isInProject(identifier string) bool {
	if t.project == "" {
		return true
	}

	lastDash := strings.LastIndex(identifier, "-")
	if lastDash < 0 {
		return true
	}

	prefix := identifier[:lastDash]
	return strings.EqualFold(prefix, t.project)
}

// mapTrackerError converts a tracker error into the failure envelope.
// Context cancellation and deadline exceeded are mapped to
// tracker_transport_error. TrackerError kinds map directly. Unknown
// errors become internal_error. The Go error return is reserved for an
// internal marshal failure.
func mapTrackerError(err error) (json.RawMessage, error) {
	if errors.Is(err, context.Canceled) {
		return toolresult.Failure("tracker_transport_error", "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return toolresult.Failure("tracker_transport_error", "deadline exceeded")
	}

	if te, ok := errors.AsType[*domain.TrackerError](err); ok {
		return toolresult.Failure(string(te.Kind), te.Message)
	}

	return toolresult.Failure("internal_error", "an unexpected internal error occurred")
}
