package domain

import "context"

// IssueKind is the planning kind of a tracker issue. Each adapter maps
// it to its tracker's own model (an issue type, a task type, a label)
// and documents that mapping in its adapter notes.
type IssueKind string

const (
	// IssueKindEpic is an issue that groups stories: one per planned change.
	IssueKindEpic IssueKind = "epic"

	// IssueKindStory is one deliverable, parented to its epic.
	IssueKindStory IssueKind = "story"
)

// Valid reports whether k is one of the declared kinds.
func (k IssueKind) Valid() bool {
	return k == IssueKindEpic || k == IssueKindStory
}

// NewIssue is the input to [IssueCreator.CreateIssue].
type NewIssue struct {
	// Title is the issue's title. Required.
	Title string

	// Body is the issue's description in Markdown. Adapters convert
	// it to their native format.
	Body string

	// Kind is the planning kind. Required.
	Kind IssueKind

	// PlanKey is the stable key of the plan node the issue comes from.
	// Required. The adapter stores it so that [PlanKeyFinder] finds the
	// issue later, whatever people do to its title, status, or labels.
	PlanKey string

	// ParentID is the tracker-internal ID of the parent issue, empty
	// for an issue without a parent.
	ParentID string

	// State is the state name the issue starts in. Empty means the
	// tracker's own default for a new issue. An adapter returns a
	// payload error for a state the issue's type cannot hold.
	State string
}

// CreatedIssue identifies an issue that [IssueCreator.CreateIssue]
// created.
type CreatedIssue struct {
	// ID is the tracker-internal ID.
	ID string

	// Identifier is the human-readable key, e.g. "PROJ-123".
	Identifier string
}

// IssueFields names the fields [IssueUpdater.UpdateIssue] changes. A
// nil field is left unchanged.
type IssueFields struct {
	Title *string
	Body  *string
	Kind  *IssueKind
}

// IssueCreator is the optional capability to create issues in the
// configured project.
type IssueCreator interface {
	// CreateIssue creates one issue and stores its plan key. It returns
	// a [*TrackerError] on failure: transport, auth, API, not found
	// (the parent does not exist), or payload (a blank title, an
	// unknown kind, or a malformed response).
	CreateIssue(ctx context.Context, issue NewIssue) (CreatedIssue, error)
}

// IssueUpdater is the optional capability to change an issue's title,
// body, or kind.
type IssueUpdater interface {
	// UpdateIssue changes the given fields of one issue. Fields left
	// nil are not touched. It returns a [*TrackerError] on failure.
	UpdateIssue(ctx context.Context, issueID string, fields IssueFields) error
}

// IssueParenter is the optional capability to set or remove an
// issue's parent.
type IssueParenter interface {
	// SetParent makes parentID the parent of issueID, or removes the
	// parent when parentID is empty. Setting the parent the issue
	// already has succeeds and changes nothing. It returns a
	// [*TrackerError] on failure.
	SetParent(ctx context.Context, issueID, parentID string) error
}

// DependencyLinker is the optional capability to add and remove
// "blocks" relations between issues.
type DependencyLinker interface {
	// AddDependency records that blockerID blocks blockedID. Adding a
	// relation that already exists succeeds and changes nothing.
	AddDependency(ctx context.Context, blockerID, blockedID string) error

	// RemoveDependency removes the relation. Removing a relation that
	// does not exist succeeds and changes nothing.
	RemoveDependency(ctx context.Context, blockerID, blockedID string) error
}

// PlanIssue is an issue as plan sync and the plan graph read it.
type PlanIssue struct {
	// ID is the tracker-internal ID.
	ID string

	// Identifier is the human-readable key.
	Identifier string

	// PlanKey is the stored plan key, empty for an issue no plan
	// created.
	PlanKey string

	// Kind is the planning kind the tracker's own model maps to, empty
	// when the issue is neither an epic nor a story.
	Kind IssueKind

	// Title is the issue's title.
	Title string

	// BodyDigest identifies the body the last plan write stored, as
	// the hex SHA-256 of the Markdown body. Empty when unknown, which
	// a sync reads as "differs".
	BodyDigest string

	// State is the tracker state name.
	State string

	// URL is the issue's web URL, empty when the tracker has none.
	URL string

	// ParentID is the parent's tracker-internal ID, empty for none.
	ParentID string

	// BlockedBy lists the tracker-internal IDs of the issues that block
	// this one. Non-nil.
	BlockedBy []string

	// Labels lists the issue's labels as the tracker names them. Nil
	// when the adapter does not read them for plan issues.
	Labels []string

	// Approval is the approver the issue records, when the adapter
	// implements [ApprovalWriter]. Nil when none is recorded.
	Approval *PlanApproval
}

// PlanApproval records who approved a plan story, and when.
type PlanApproval struct {
	// By is the approver's display name.
	By string
	// ByID is the approver's stable identity, for example
	// "<issuer>|<subject>".
	ByID string
	// At is the approval time in RFC 3339.
	At string
}

// ApprovalWriter is the optional capability to record an issue's
// approver and approval time in fields of their own.
type ApprovalWriter interface {
	// SetApproval records a on the issue, replacing any approval it
	// records. It returns a [*TrackerError] on failure.
	SetApproval(ctx context.Context, issueID string, a PlanApproval) error
}

// PlanKeyFinder is the capability to find issues by plan key. An
// adapter that declares any write capability implements it.
type PlanKeyFinder interface {
	// FindIssueByPlanKey returns the configured project's issue that
	// carries planKey. It returns a [*TrackerError] with Kind
	// [ErrTrackerNotFound] when no issue carries it.
	FindIssueByPlanKey(ctx context.Context, planKey string) (PlanIssue, error)

	// ListPlanIssues returns every issue in the configured project that
	// carries a plan key, and also the issues without one when
	// includeUnkeyed is true. The listing is complete or it is an
	// error; it is never partial.
	ListPlanIssues(ctx context.Context, includeUnkeyed bool) ([]PlanIssue, error)
}
