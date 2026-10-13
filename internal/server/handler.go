package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/sentence"
)

// stateResponse is the JSON wire format for GET /api/v1/state.
type stateResponse struct {
	GeneratedAt            time.Time                        `json:"generated_at"`
	Version                string                           `json:"version"`
	StartedAt              *time.Time                       `json:"started_at"`
	Counts                 stateCounts                      `json:"counts"`
	Running                []runningEntryResponse           `json:"running"`
	Retrying               []retryEntryResponse             `json:"retrying"`
	BudgetExhausted        []budgetExhaustedResponse        `json:"budget_exhausted"`
	Parked                 []parkedEntryResponse            `json:"parked"`
	PendingReactions       []pendingReactionResponse        `json:"pending_reactions"`
	BetweenStages          []stageHopResponse               `json:"between_stages"`
	AgentTotals            orchestrator.SnapshotAgentTotals `json:"agent_totals"`
	RateLimits             map[string]any                   `json:"rate_limits"`
	ActiveEstimatedCostUSD *float64                         `json:"active_estimated_cost_usd,omitempty"`

	// CostUnpricedRunning counts running, measured sessions
	// ActiveEstimatedCostUSD excludes for lacking a configured rate.
	// Present, zero included, when any agent kind has a rate
	// configured; omitted otherwise.
	CostUnpricedRunning *int `json:"cost_unpriced_running,omitempty"`

	Config *configResponse `json:"config,omitempty"`

	Health *stateHealthResponse `json:"health,omitempty"`
}

type stateCounts struct {
	Running          int `json:"running"`
	Retrying         int `json:"retrying"`
	BudgetExhausted  int `json:"budget_exhausted"`
	Parked           int `json:"parked"`
	PendingReactions int `json:"pending_reactions"`
	BetweenStages    int `json:"between_stages"`
}

type runningEntryResponse struct {
	IssueID             string                 `json:"issue_id"`
	IssueIdentifier     string                 `json:"issue_identifier"`
	DisplayIdentifier   string                 `json:"display_identifier,omitempty"`
	IssueTitle          string                 `json:"issue_title,omitempty"`
	IssueURL            string                 `json:"issue_url,omitempty"`
	IssueLabels         []string               `json:"issue_labels"`
	IssueType           string                 `json:"issue_type,omitempty"`
	State               string                 `json:"state"`
	SessionID           string                 `json:"session_id"`
	TurnCount           int                    `json:"turn_count"`
	SelfReviewActive    bool                   `json:"self_review_active"`
	SelfReviewIteration int                    `json:"self_review_iteration"`
	LastEvent           string                 `json:"last_event"`
	LastMessage         string                 `json:"last_message"`
	StartedAt           time.Time              `json:"started_at"`
	DispatchedAt        *time.Time             `json:"dispatched_at"`
	LastEventAt         time.Time              `json:"last_event_at"`
	WorkspacePath       string                 `json:"workspace_path"`
	SSHHost             string                 `json:"ssh_host,omitempty"`
	Tokens              tokenInfo              `json:"tokens"`
	ModelName           string                 `json:"model_name,omitempty"`
	RuleName            string                 `json:"rule_name,omitempty"`
	StageLineage        *stageLineageResponse  `json:"stage_lineage"`
	StagePosition       *stagePositionResponse `json:"stage_position"`
	ConfiguredModel     string                 `json:"configured_model,omitempty"`
	ConfiguredEffort    string                 `json:"configured_effort,omitempty"`
	WorkflowFile        string                 `json:"workflow_file,omitempty"`
	AgentKind           string                 `json:"agent_kind,omitempty"`
	ReactionKind        string                 `json:"reaction_kind,omitempty"`
	ReactionConfigKey   string                 `json:"reaction_config_key,omitempty"`
	ReactionAttempt     int                    `json:"reaction_attempt,omitempty"`
	PullRequest         *pullRequestResponse   `json:"pull_request,omitempty"`
	APIRequestCount     *int                   `json:"api_request_count"`
	RequestsByModel     map[string]int         `json:"requests_by_model,omitempty"`
	ToolTimePercent     *float64               `json:"tool_time_percent"`
	APITimePercent      *float64               `json:"api_time_percent"`
	TokensMeasured      bool                   `json:"tokens_measured"`

	// UsageArrival and UsageAttribution mirror
	// [orchestrator.SnapshotRunningEntry]'s frozen pair verbatim, as
	// their string values. TokensPending derives from that frozen
	// pair; APIRequestsMeasured is the session-resolved verdict the
	// snapshot carries, and it is true exactly when APIRequestCount
	// is non-nil.
	UsageArrival        string `json:"usage_arrival"`
	UsageAttribution    string `json:"usage_attribution"`
	TokensPending       bool   `json:"tokens_pending"`
	TokensAwaited       bool   `json:"tokens_awaited"`
	APIRequestsMeasured bool   `json:"api_requests_measured"`

	// The issue-level figures below are nil when unknown. The two budget
	// keys are nil when no limit is configured and are never omitted.
	UsedSessions          *int     `json:"used_sessions"`
	BudgetSessions        *int     `json:"budget_sessions"`
	IssueTokensCompleted  *int64   `json:"issue_tokens_completed"`
	UnmeasuredSessions    *int     `json:"unmeasured_sessions"`
	UsedTokens            *int64   `json:"used_tokens"`
	UsedTokensComplete    bool     `json:"used_tokens_complete"`
	BudgetTokens          *int64   `json:"budget_tokens"`
	EstimatedCostUSD      *float64 `json:"estimated_cost_usd"`
	CostStatus            string   `json:"cost_status"`
	IssueEstimatedCostUSD *float64 `json:"issue_estimated_cost_usd"`
	IssueCostStatus       string   `json:"issue_cost_status"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// tokenInfo carries a running row's five token figures. The members
// are nil together, exactly when the row's TokensMeasured is false,
// and a nil member serializes as JSON null rather than being omitted,
// so a consumer can tell a measured zero from an unmeasured figure.
type tokenInfo struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

type retryEntryResponse struct {
	IssueID           string     `json:"issue_id"`
	IssueIdentifier   string     `json:"issue_identifier"`
	DisplayIdentifier string     `json:"display_identifier,omitempty"`
	IssueTitle        string     `json:"issue_title,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	IssueType         string     `json:"issue_type,omitempty"`
	IssueLabels       []string   `json:"issue_labels,omitzero"`
	State             string     `json:"state,omitempty"`
	DispatchedAt      *time.Time `json:"dispatched_at"`
	Attempt           int        `json:"attempt"`
	DueAt             time.Time  `json:"due_at"`
	Error             string     `json:"error"`

	Trigger           string                 `json:"trigger,omitempty"`
	ReactionKind      string                 `json:"reaction_kind,omitempty"`
	ReactionConfigKey string                 `json:"reaction_config_key,omitempty"`
	RuleName          string                 `json:"rule_name,omitempty"`
	AgentKind         string                 `json:"agent_kind,omitempty"`
	ConfiguredModel   string                 `json:"configured_model,omitempty"`
	StageLineage      *stageLineageResponse  `json:"stage_lineage"`
	StagePosition     *stagePositionResponse `json:"stage_position"`
	Paused            bool                   `json:"paused"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// stageLineageResponse is the stage hop that reached the run a row
// describes. It is nil in the wire format unless a hop reached that run.
type stageLineageResponse struct {
	ChainID         string `json:"chain_id,omitempty"`
	Previous        string `json:"previous,omitempty"`
	PreviousOutcome string `json:"previous_outcome,omitempty"`
}

// stagePositionResponse is where a row's rule stands on its stage chain. It
// is nil in the wire format when the rule is on no chain.
type stagePositionResponse struct {
	Chain      []string `json:"chain"`
	Current    string   `json:"current"`
	HopCount   int      `json:"hop_count"`
	HopCeiling int      `json:"hop_ceiling"`
}

// stageHopResponse is the JSON wire format for one issue between stages.
// Used by GET /api/v1/state and GET /api/v1/{identifier}.
type stageHopResponse struct {
	IssueID           string     `json:"issue_id"`
	IssueIdentifier   string     `json:"issue_identifier"`
	DisplayIdentifier string     `json:"display_identifier,omitempty"`
	IssueTitle        string     `json:"issue_title,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	IssueType         string     `json:"issue_type,omitempty"`
	IssueLabels       []string   `json:"issue_labels,omitzero"`
	State             string     `json:"state,omitempty"`
	SourceRule        string     `json:"source_rule"`
	TargetRule        string     `json:"target_rule"`
	TargetLabel       string     `json:"target_label"`
	HoppedAt          *time.Time `json:"hopped_at"`
	DispatchedAt      *time.Time `json:"dispatched_at"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *stagePositionResponse `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// parkedEntryResponse is the JSON wire format for one issue held out of
// primary dispatch until a person acts on it. Used by GET /api/v1/state and
// GET /api/v1/{identifier}.
type parkedEntryResponse struct {
	IssueID           string     `json:"issue_id"`
	IssueIdentifier   string     `json:"issue_identifier"`
	DisplayIdentifier string     `json:"display_identifier,omitempty"`
	IssueTitle        string     `json:"issue_title,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	IssueType         string     `json:"issue_type,omitempty"`
	IssueLabels       []string   `json:"issue_labels,omitzero"`
	State             string     `json:"state,omitempty"`
	DispatchedAt      *time.Time `json:"dispatched_at"`
	Reason            string     `json:"reason"`
	ParkedState       string     `json:"parked_state,omitempty"`
	Label             string     `json:"label"`
	LabelApplied      bool       `json:"label_applied"`
	ParkedAt          time.Time  `json:"parked_at"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *stagePositionResponse `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// pendingReactionResponse is the JSON wire format for one pending pull
// request reaction. Used by GET /api/v1/state and GET /api/v1/{identifier}.
type pendingReactionResponse struct {
	IssueID           string                 `json:"issue_id"`
	IssueIdentifier   string                 `json:"issue_identifier"`
	DisplayIdentifier string                 `json:"display_identifier,omitempty"`
	IssueTitle        string                 `json:"issue_title,omitempty"`
	IssueURL          string                 `json:"issue_url,omitempty"`
	IssueType         string                 `json:"issue_type,omitempty"`
	IssueLabels       []string               `json:"issue_labels,omitzero"`
	State             string                 `json:"state,omitempty"`
	Kind              string                 `json:"kind"`
	ConfigKey         string                 `json:"config_key"`
	PullRequest       pullRequestResponse    `json:"pull_request"`
	CreatedAt         time.Time              `json:"created_at"`
	DispatchedAt      *time.Time             `json:"dispatched_at"`
	NextPollAt        time.Time              `json:"next_poll_at"`
	Wait              string                 `json:"wait,omitempty"`
	UsedAttempts      *int                   `json:"used_attempts"`
	MaxAttempts       *int                   `json:"max_attempts"`
	EscalatedAt       *time.Time             `json:"escalated_at"`
	LastCheck         *reactionCheckResponse `json:"last_check"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *stagePositionResponse `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// pullRequestResponse is the JSON wire format for the pull request a pending
// reaction watches.
type pullRequestResponse struct {
	Number     int    `json:"number"`
	Repository string `json:"repository"`
	URL        string `json:"url,omitempty"`
	Branch     string `json:"branch,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
}

// reactionCheckResponse is the JSON wire format for what a pending
// reaction's latest check observed. It serializes the members of the kind's
// result as one flat object, which struct tags cannot express: a count of
// zero and a mergeable of null must appear for the kinds that record them
// and not at all for the others.
type reactionCheckResponse orchestrator.ReactionCheck

type reactionCheckHeader struct {
	ObservedAt time.Time `json:"observed_at"`
	HeadSHA    string    `json:"head_sha,omitempty"`
}

// MarshalJSON writes observed_at, then head_sha when the check read a head,
// then the members of the one result the check holds.
func (c reactionCheckResponse) MarshalJSON() ([]byte, error) {
	header := reactionCheckHeader{ObservedAt: c.ObservedAt.UTC(), HeadSHA: c.HeadSHA}
	switch {
	case c.CI != nil:
		failingChecks := c.CI.FailingChecks
		if failingChecks == nil {
			failingChecks = []string{}
		}
		return json.Marshal(struct {
			reactionCheckHeader
			Status        string   `json:"status"`
			FailingChecks []string `json:"failing_checks"`
			Failing       int      `json:"failing"`
			Pending       int      `json:"pending"`
			Passing       int      `json:"passing"`
		}{header, c.CI.Status, failingChecks, c.CI.Failing, c.CI.Pending, c.CI.Passing})
	case c.Comments != nil:
		return json.Marshal(struct {
			reactionCheckHeader
			ActionableComments int `json:"actionable_comments"`
			NewComments        int `json:"new_comments"`
		}{header, c.Comments.Actionable, c.Comments.New})
	case c.Mergeability != nil:
		return json.Marshal(struct {
			reactionCheckHeader
			Mergeable *bool `json:"mergeable"`
		}{header, c.Mergeability.Mergeable})
	default:
		return json.Marshal(header)
	}
}

// budgetExhaustedResponse is the JSON wire format for one issue held out
// of dispatch by a budget ceiling. Used by GET /api/v1/state and
// GET /api/v1/{identifier}.
type budgetExhaustedResponse struct {
	IssueID            string                 `json:"issue_id"`
	IssueIdentifier    string                 `json:"issue_identifier"`
	DisplayIdentifier  string                 `json:"display_identifier,omitempty"`
	IssueTitle         string                 `json:"issue_title,omitempty"`
	IssueURL           string                 `json:"issue_url,omitempty"`
	IssueType          string                 `json:"issue_type,omitempty"`
	IssueLabels        []string               `json:"issue_labels,omitzero"`
	State              string                 `json:"state,omitempty"`
	DispatchedAt       *time.Time             `json:"dispatched_at"`
	Reason             string                 `json:"reason"`
	UsedSessions       int                    `json:"used_sessions"`
	BudgetSessions     *int                   `json:"budget_sessions"`
	UsedTokens         *int64                 `json:"used_tokens"`
	BudgetTokens       *int64                 `json:"budget_tokens"`
	UnmeasuredSessions *int                   `json:"unmeasured_sessions"`
	StoppedInFlight    *int                   `json:"stopped_in_flight"`
	ExhaustedAt        time.Time              `json:"exhausted_at"`
	RuleName           string                 `json:"rule_name,omitempty"`
	StageLineage       *stageLineageResponse  `json:"stage_lineage"`
	StagePosition      *stagePositionResponse `json:"stage_position"`
	AgentKind          string                 `json:"agent_kind,omitempty"`
	ConfiguredModel    string                 `json:"configured_model,omitempty"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

// issueResponse is the tracker's view of one issue, under the names the
// prompt template's issue variable uses. A member is omitted when its source
// holds none; Labels is nil only when the source holds no read of the labels.
type issueResponse struct {
	Title     string   `json:"title,omitempty"`
	URL       string   `json:"url,omitempty"`
	IssueType string   `json:"issue_type,omitempty"`
	Labels    []string `json:"labels,omitzero"`
	State     string   `json:"state,omitempty"`
}

type issueDetailResponse struct {
	IssueIdentifier  string                    `json:"issue_identifier"`
	IssueID          string                    `json:"issue_id"`
	Status           string                    `json:"status"`
	Issue            issueResponse             `json:"issue"`
	Workspace        *workspaceInfo            `json:"workspace"`
	Attempts         *attemptsInfo             `json:"attempts"`
	Running          *runningEntryResponse     `json:"running"`
	Retry            *retryEntryResponse       `json:"retry"`
	BudgetExhausted  *budgetExhaustedResponse  `json:"budget_exhausted"`
	Parked           *parkedEntryResponse      `json:"parked"`
	PendingReactions []pendingReactionResponse `json:"pending_reactions"`
	BetweenStages    *stageHopResponse         `json:"between_stages"`
	RecentEvents     []any                     `json:"recent_events"`
	LastError        *string                   `json:"last_error"`
	Tracked          map[string]any            `json:"tracked"`
	History          *issueHistoryResponse     `json:"history"`

	StatusText     string `json:"status_text"`
	StatusTextFull string `json:"status_text_full"`
}

type workspaceInfo struct {
	Path string `json:"path"`
}

type attemptsInfo struct {
	RestartCount        int `json:"restart_count"`
	CurrentRetryAttempt int `json:"current_retry_attempt"`
}

type refreshResponse struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
	OutcomeText string    `json:"outcome_text"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func toRunningEntryResponse(e orchestrator.SnapshotRunningEntry, nowArgs ...time.Time) runningEntryResponse {
	resp := runningEntryResponse{
		IssueID:             e.IssueID,
		IssueIdentifier:     e.Identifier,
		DisplayIdentifier:   e.DisplayID,
		IssueTitle:          e.IssueTitle,
		IssueURL:            e.IssueURL,
		IssueLabels:         e.IssueLabels,
		IssueType:           e.IssueType,
		State:               e.State,
		SessionID:           e.SessionID,
		TurnCount:           e.TurnCount,
		SelfReviewActive:    e.SelfReviewActive,
		SelfReviewIteration: e.SelfReviewIteration,
		LastEvent:           string(e.LastAgentEvent),
		LastMessage:         e.LastAgentMessage,
		StartedAt:           e.StartedAt.UTC(),
		DispatchedAt:        utcOrNil(e.StageLineage.DispatchedAt),
		LastEventAt:         e.LastAgentTimestamp.UTC(),
		WorkspacePath:       e.WorkspacePath,
		SSHHost:             e.SSHHost,
		ModelName:           e.ModelName,
		RuleName:            e.RuleName,
		StageLineage:        toStageLineageResponse(e.StageLineage),
		StagePosition:       toStagePositionResponse(e.StagePosition),
		ConfiguredModel:     e.ConfiguredModel,
		ConfiguredEffort:    e.ConfiguredEffort,
		WorkflowFile:        e.WorkflowFile,
		AgentKind:           e.AgentKind,
		ReactionKind:        e.ReactionKind,
		ReactionConfigKey:   e.ReactionConfigKey,
		RequestsByModel:     e.RequestsByModel,
		TokensMeasured:      e.UsageMeasured,
		UsageArrival:        string(e.UsageArrival),
		UsageAttribution:    string(e.UsageAttribution),
		TokensPending:       e.TokensPending,
		TokensAwaited:       e.TokensAwaited,
		APIRequestsMeasured: e.APIRequestsMeasured,
	}

	// A figure no measurement produced is null in the place it already
	// occupies, beside the flag that explains it.
	if e.UsageMeasured {
		resp.Tokens = tokenInfo{
			InputTokens:      &e.AgentInputTokens,
			OutputTokens:     &e.AgentOutputTokens,
			TotalTokens:      &e.AgentTotalTokens,
			CacheReadTokens:  &e.CacheReadTokens,
			CacheWriteTokens: &e.CacheWriteTokens,
		}
	}
	if e.APIRequestsMeasured {
		resp.APIRequestCount = &e.APIRequestCount
	}
	if e.Continuation != nil {
		resp.ReactionAttempt = e.Continuation.Attempt
		resp.PullRequest = &pullRequestResponse{
			Number:     e.Continuation.Number,
			Repository: e.Continuation.Repository,
		}
	}

	if len(nowArgs) > 0 && !e.StartedAt.IsZero() {
		now := nowArgs[0]
		elapsedMs := now.Sub(e.StartedAt).Milliseconds()
		if elapsedMs > 0 && e.ToolTimeMs > 0 {
			resp.ToolTimePercent = new(float64(e.ToolTimeMs) / float64(elapsedMs) * 100.0)
		}
		if elapsedMs > 0 && e.APITimeMs > 0 {
			resp.APITimePercent = new(float64(e.APITimeMs) / float64(elapsedMs) * 100.0)
		}
	}

	return resp
}

// toServedRunningEntry converts a running row for both endpoints, adding
// the issue-level figures that depend on the snapshot's limits and on rates.
func toServedRunningEntry(e orchestrator.SnapshotRunningEntry, snap orchestrator.RuntimeSnapshotResult, rates TokenRates) runningEntryResponse {
	resp := toRunningEntryResponse(e, snap.GeneratedAt)

	var completed int64
	var completedIncomplete bool
	if usage := e.IssueUsage; usage != nil {
		completed = usage.TotalTokens
		completedIncomplete = orchestrator.SpendIncomplete(usage.UnmeasuredSessions, usage.UnaccountedTurns)
		resp.UsedSessions = new(usage.Sessions)
		resp.UnmeasuredSessions = new(usage.UnmeasuredSessions)
	}

	known := completed
	if e.UsageMeasured {
		known += e.AgentTotalTokens
	}
	resp.UsedTokensComplete = e.IssueUsage != nil && !completedIncomplete && e.UsageMeasured && e.UnaccountedTurns == 0

	// A zero that leaves spend out would stand in for spend Sortie does not
	// know, so it is served as null.
	if e.IssueUsage != nil && (completed != 0 || !completedIncomplete) {
		resp.IssueTokensCompleted = new(completed)
	}
	if known != 0 || resp.UsedTokensComplete {
		resp.UsedTokens = new(known)
	}

	if snap.MaxTokens > 0 {
		resp.BudgetTokens = new(int64(snap.MaxTokens))
	}
	if snap.MaxSessions > 0 {
		resp.BudgetSessions = new(snap.MaxSessions)
	}

	resp.EstimatedCostUSD, resp.CostStatus = sessionCostOf(e, rates)
	resp.IssueEstimatedCostUSD, resp.IssueCostStatus = issueCostOf(e, resp.UsedTokens, resp.UsedTokensComplete, resp.EstimatedCostUSD, resp.CostStatus, rates)
	return resp
}

// utcOrNil returns the UTC value of at, or nil for the zero time, so a time
// no record could date serializes as null.
func utcOrNil(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return new(at.UTC())
}

// toStageLineageResponse returns nil unless a stage hop reached the run, so
// a chain identifier alone never makes a row claim a lineage.
func toStageLineageResponse(l orchestrator.StageLineage) *stageLineageResponse {
	if l.PreviousRule == "" {
		return nil
	}
	return &stageLineageResponse{
		ChainID:         l.ChainID,
		Previous:        l.PreviousRule,
		PreviousOutcome: l.PreviousOutcome,
	}
}

func toStagePositionResponse(p *orchestrator.SnapshotStagePosition) *stagePositionResponse {
	if p == nil {
		return nil
	}
	return &stagePositionResponse{
		Chain:      p.Chain,
		Current:    p.Current,
		HopCount:   p.HopCount,
		HopCeiling: p.HopCeiling,
	}
}

func toStageHopResponse(e orchestrator.SnapshotStageHop) stageHopResponse {
	return stageHopResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		SourceRule:        e.SourceRule,
		TargetRule:        e.TargetRule,
		TargetLabel:       e.TargetLabel,
		HoppedAt:          utcOrNil(e.HoppedAt),
		DispatchedAt:      utcOrNil(e.DispatchedAt),

		RuleName:        e.RuleName,
		StagePosition:   toStagePositionResponse(e.StagePosition),
		AgentKind:       e.AgentKind,
		ConfiguredModel: e.ConfiguredModel,
	}
}

func toRetryEntryResponse(e orchestrator.SnapshotRetryEntry) retryEntryResponse {
	return retryEntryResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		DispatchedAt:      utcOrNil(e.StageLineage.DispatchedAt),
		Attempt:           e.Attempt,
		DueAt:             time.UnixMilli(e.DueAtMS).UTC(),
		Error:             e.Error,

		Trigger:           e.Trigger,
		ReactionKind:      e.ReactionKind,
		ReactionConfigKey: e.ReactionConfigKey,
		RuleName:          e.RuleName,
		AgentKind:         e.AgentKind,
		ConfiguredModel:   e.ConfiguredModel,
		StageLineage:      toStageLineageResponse(e.StageLineage),
		StagePosition:     toStagePositionResponse(e.StagePosition),
		Paused:            e.Paused,
	}
}

func toParkedEntryResponse(e orchestrator.SnapshotParkedEntry) parkedEntryResponse {
	return parkedEntryResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.ParkedState,
		DispatchedAt:      utcOrNil(e.DispatchedAt),
		Reason:            e.Reason,
		ParkedState:       e.ParkedState,
		Label:             e.Label,
		LabelApplied:      e.LabelApplied,
		ParkedAt:          e.ParkedAt.UTC(),

		RuleName:        e.RuleName,
		StagePosition:   toStagePositionResponse(e.StagePosition),
		AgentKind:       e.AgentKind,
		ConfiguredModel: e.ConfiguredModel,
	}
}

func toPendingReactionResponse(e orchestrator.SnapshotPendingReaction) pendingReactionResponse {
	resp := pendingReactionResponse{
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		IssueType:         e.IssueType,
		IssueLabels:       e.IssueLabels,
		State:             e.State,
		Kind:              e.Kind,
		ConfigKey:         e.ConfigKey,
		PullRequest: pullRequestResponse{
			Number:     e.PullRequest.Number,
			Repository: e.PullRequest.Repository,
			URL:        e.PullRequest.URL,
			Branch:     e.PullRequest.Branch,
			HeadSHA:    e.PullRequest.HeadSHA,
		},
		CreatedAt:    e.CreatedAt.UTC(),
		DispatchedAt: utcOrNil(e.DispatchedAt),
		NextPollAt:   e.NextPollAt.UTC(),
		Wait:         e.Wait,
		UsedAttempts: e.UsedAttempts,
		MaxAttempts:  e.MaxAttempts,
		LastCheck:    (*reactionCheckResponse)(e.LastCheck),

		RuleName:        e.RuleName,
		StagePosition:   toStagePositionResponse(e.StagePosition),
		AgentKind:       e.AgentKind,
		ConfiguredModel: e.ConfiguredModel,
	}
	if e.EscalatedAt != nil {
		resp.EscalatedAt = new(e.EscalatedAt.UTC())
	}
	return resp
}

func toPendingReactionResponses(rows []orchestrator.SnapshotPendingReaction, words sentence.Context) []pendingReactionResponse {
	responses := make([]pendingReactionResponse, 0, len(rows))
	for _, e := range rows {
		row := toPendingReactionResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{PendingReaction: &e}, words)
		responses = append(responses, row)
	}
	return responses
}

// sentencesOf returns the short and long status sentence of one entry.
func sentencesOf(entry sentence.Entry, words sentence.Context) (short, long string) {
	status := sentence.For(entry, words)
	return status.Short, status.Long
}

func toBudgetExhaustedResponse(e orchestrator.SnapshotBudgetEntry) budgetExhaustedResponse {
	var budgetSessions *int
	if e.BudgetSessions > 0 {
		budgetSessions = new(e.BudgetSessions)
	}
	var budgetTokens *int64
	if e.BudgetTokens > 0 {
		budgetTokens = new(e.BudgetTokens)
	}
	return budgetExhaustedResponse{
		IssueID:            e.IssueID,
		IssueIdentifier:    e.Identifier,
		DisplayIdentifier:  e.DisplayID,
		IssueTitle:         e.IssueTitle,
		IssueURL:           e.IssueURL,
		IssueType:          e.IssueType,
		IssueLabels:        e.IssueLabels,
		State:              e.State,
		DispatchedAt:       utcOrNil(e.StageLineage.DispatchedAt),
		Reason:             e.Reason,
		UsedSessions:       e.UsedSessions,
		BudgetSessions:     budgetSessions,
		UsedTokens:         heldUsedTokens(e),
		BudgetTokens:       budgetTokens,
		UnmeasuredSessions: e.UnmeasuredSessions,
		StoppedInFlight:    e.StoppedInFlight,
		ExhaustedAt:        e.ExhaustedAt,
		RuleName:           e.RuleName,
		StageLineage:       toStageLineageResponse(e.StageLineage),
		StagePosition:      toStagePositionResponse(e.StagePosition),
		AgentKind:          e.AgentKind,
		ConfiguredModel:    e.ConfiguredModel,
	}
}

// heldUsedTokens returns the held issue's token sum, or nil when the sum is
// unread or is a zero that leaves spend out: that zero would stand in for
// spend Sortie does not know.
func heldUsedTokens(e orchestrator.SnapshotBudgetEntry) *int64 {
	if e.UsedTokens == nil {
		return nil
	}
	var unmeasured int
	if e.UnmeasuredSessions != nil {
		unmeasured = *e.UnmeasuredSessions
	}
	if *e.UsedTokens == 0 && orchestrator.SpendIncomplete(unmeasured, e.UnaccountedTurns) {
		return nil
	}
	return e.UsedTokens
}

func toStateResponse(snap orchestrator.RuntimeSnapshotResult, tokenRates TokenRates, words sentence.Context) stateResponse {
	running := make([]runningEntryResponse, 0, len(snap.Running))
	for _, e := range snap.Running {
		row := toServedRunningEntry(e, snap, tokenRates)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Running: &e}, words)
		running = append(running, row)
	}

	retrying := make([]retryEntryResponse, 0, len(snap.Retrying))
	for _, e := range snap.Retrying {
		row := toRetryEntryResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Retry: &e}, words)
		retrying = append(retrying, row)
	}

	budgetExhausted := make([]budgetExhaustedResponse, 0, len(snap.BudgetExhausted))
	for _, e := range snap.BudgetExhausted {
		row := toBudgetExhaustedResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{BudgetHold: &e}, words)
		budgetExhausted = append(budgetExhausted, row)
	}

	parked := make([]parkedEntryResponse, 0, len(snap.Parked))
	for _, e := range snap.Parked {
		row := toParkedEntryResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{Parked: &e}, words)
		parked = append(parked, row)
	}

	pendingReactions := toPendingReactionResponses(snap.PendingReactions, words)

	betweenStages := make([]stageHopResponse, 0, len(snap.BetweenStages))
	for _, e := range snap.BetweenStages {
		row := toStageHopResponse(e)
		row.StatusText, row.StatusTextFull = sentencesOf(sentence.Entry{BetweenStages: &e}, words)
		betweenStages = append(betweenStages, row)
	}

	rateLimits := snap.RateLimits
	if rateLimits == nil {
		rateLimits = map[string]any{}
	}

	resp := stateResponse{
		GeneratedAt: snap.GeneratedAt.UTC(),
		Counts: stateCounts{
			Running:          len(running),
			Retrying:         len(retrying),
			BudgetExhausted:  len(budgetExhausted),
			Parked:           len(parked),
			PendingReactions: len(pendingReactions),
			BetweenStages:    len(betweenStages),
		},
		Running:          running,
		Retrying:         retrying,
		BudgetExhausted:  budgetExhausted,
		Parked:           parked,
		PendingReactions: pendingReactions,
		BetweenStages:    betweenStages,
		AgentTotals:      snap.AgentTotals,
		RateLimits:       rateLimits,
	}

	if len(tokenRates) > 0 {
		total, anySet, unpriced := activeCostTotal(snap.Running, tokenRates)
		if anySet {
			resp.ActiveEstimatedCostUSD = &total
		}
		resp.CostUnpricedRunning = &unpriced
	}

	return resp
}

func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		logger.Error("failed to marshal JSON response", slog.Any("error", err))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"failed to encode response"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf.Bytes()); err != nil {
		logger.Warn("failed to write JSON response", slog.Any("error", err))
	}
}

func writeErrorJSON(w http.ResponseWriter, logger *slog.Logger, status int, code string, message string) {
	writeJSON(w, logger, status, errorResponse{
		Error: errorDetail{Code: code, Message: message},
	})
}

func (s *Server) routeState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleState(w, r)
}

func (s *Server) routeRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w, r, http.MethodPost)
		return
	}
	s.handleRefresh(w, r)
}

func (s *Server) routeIssueDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w, r, http.MethodGet)
		return
	}
	s.handleIssueDetail(w, r)
}

// sentenceContext reads the workflow shape once for a request, so the
// sentences and the config the state route serves never disagree.
func (s *Server) sentenceContext(snap orchestrator.RuntimeSnapshotResult) sentence.Context {
	words := sentence.Context{Now: snap.GeneratedAt}
	if s.workflowShapeFn != nil {
		shape := s.workflowShapeFn()
		words.Shape = &shape
	}
	return words
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	snap, err := s.snapshotFn()
	if err != nil {
		s.logger.Warn("snapshot request failed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable,
			"snapshot_unavailable",
			"orchestrator state snapshot unavailable")
		return
	}

	words := s.sentenceContext(snap)
	resp := toStateResponse(snap, s.tokenRates, words)
	resp.Version = s.version
	resp.StartedAt = utcOrNil(s.startedAt)
	if words.Shape != nil {
		resp.Config = toConfigResponse(*words.Shape, s.tokenRates)
	}
	if s.healthFn != nil {
		resp.Health = toStateHealthResponse(s.healthFn())
	}
	writeJSON(w, s.logger, http.StatusOK, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) handleIssueDetail(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	identifier := r.PathValue("identifier")
	if identifier == "" {
		writeErrorJSON(w, s.logger, http.StatusNotFound,
			"issue_not_found",
			"issue identifier is empty")
		return
	}

	snap, err := s.snapshotFn()
	if err != nil {
		s.logger.Warn("snapshot request failed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		)
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable,
			"snapshot_unavailable",
			"orchestrator state snapshot unavailable")
		return
	}

	words := s.sentenceContext(snap)
	detail := buildIssueDetail(identifier, "", snap, s.tokenRates, words)
	notFoundIn := "current state"
	if s.runHistory != nil {
		notFoundIn = "current state or run history"
		var ok bool
		if detail, ok = s.withIssueHistory(w, r, identifier, snap, detail, words); !ok {
			return
		}
	}
	if detail == nil {
		writeErrorJSON(w, s.logger, http.StatusNotFound,
			"issue_not_found",
			fmt.Sprintf("issue identifier %q not found in %s", identifier, notFoundIn))
		return
	}

	writeJSON(w, s.logger, http.StatusOK, detail)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusOK),
		slog.Duration("duration", time.Since(start)),
	)
}

// withIssueHistory resolves identifier through run history and attaches the
// issue's history to detail, the live detail or nil. It returns a nil detail
// for an identifier history does not know either, and false after it has
// answered a failed read itself. The snapshot is read before run history, so
// a run that ends between the reads can appear in both but never in neither.
func (s *Server) withIssueHistory(w http.ResponseWriter, r *http.Request, identifier string, snap orchestrator.RuntimeSnapshotResult, detail *issueDetailResponse, words sentence.Context) (*issueDetailResponse, bool) {
	ctx := r.Context()

	var issueID string
	if detail != nil {
		issueID = detail.IssueID
	} else {
		newest, err := s.runHistory.QueryRuns(ctx, RunHistoryQuery{Identifier: identifier, Limit: 1})
		if err != nil {
			s.historyReadFailed(w, r, err)
			return nil, false
		}
		if len(newest) == 0 {
			return nil, true
		}
		issueID = newest[0].IssueID
		detail = buildIssueDetail(identifier, issueID, snap, s.tokenRates, words)
	}

	page, err := s.runHistory.QueryRuns(ctx, RunHistoryQuery{IssueID: issueID, Limit: issueHistoryPageSize + 1})
	if err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	if len(page) == 0 {
		return detail, true
	}

	totals, err := s.runHistory.IssueTotals(ctx, issueID)
	if err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	outcomes, err := s.runHistory.IssueOutcomes(ctx, []string{issueID})
	if err != nil {
		s.historyReadFailed(w, r, err)
		return nil, false
	}
	newest := page[0]
	outcome, hasOutcome := outcomes[issueID]
	if detail == nil {
		detail = historyOnlyDetail(issueID, newest)
		detail.StatusText, detail.StatusTextFull = sentencesOf(sentence.Entry{Finished: finishedOf(newest, outcome, hasOutcome)}, words)
	}
	detail.History = issueHistoryOf(page, totals, s.tokenRates)
	attachIssueOutcomes(detail.History.Runs, outcomes)
	if hasOutcome {
		detail.History.IssueOutcome = toIssueOutcomeResponse(outcome, newest.Identifier, newest.DisplayID)
	}
	return detail, true
}

// historyOnlyDetail is the response for an issue that only run history
// knows: no live member, and the title and link of its newest run.
func historyOnlyDetail(issueID string, newest RunHistoryEntry) *issueDetailResponse {
	return &issueDetailResponse{
		IssueIdentifier:  newest.Identifier,
		IssueID:          issueID,
		Status:           "history",
		Issue:            issueResponse{Title: redact.Mask(newest.IssueTitle), URL: redact.Mask(newest.IssueURL)},
		Attempts:         &attemptsInfo{},
		PendingReactions: []pendingReactionResponse{},
		RecentEvents:     []any{},
		Tracked:          map[string]any{},
	}
}

// issueHistoryOf builds the history member from an issue's newest runs, read
// one past issueHistoryPageSize so a longer history names its continuation.
func issueHistoryOf(page []RunHistoryEntry, totals IssueRunTotals, rates TokenRates) *issueHistoryResponse {
	rows, next := cutPage(page, issueHistoryPageSize)
	return &issueHistoryResponse{
		Runs:       toRunResponses(rows, rates),
		NextCursor: next,
		Totals:     historyTotalsOf(totals, rates),
		Hops:       hopRowsOf(rows),
	}
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if s.drainingFlag.Load() {
		resp := refreshResponse{
			Queued:      false,
			Coalesced:   false,
			RequestedAt: time.Now().UTC(),
			Operations:  []string{},
		}
		resp.OutcomeText = sentence.RefreshOutcome(resp.Queued, resp.Coalesced)
		writeJSON(w, s.logger, http.StatusConflict, resp)

		s.logger.Debug("request served",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", http.StatusConflict),
			slog.Duration("duration", time.Since(start)),
		)
		return
	}

	accepted := s.refreshFn()

	resp := refreshResponse{
		Queued:      true,
		Coalesced:   !accepted,
		RequestedAt: time.Now().UTC(),
		Operations:  []string{"poll", "reconcile"},
	}
	resp.OutcomeText = sentence.RefreshOutcome(resp.Queued, resp.Coalesced)

	writeJSON(w, s.logger, http.StatusAccepted, resp)

	s.logger.Debug("request served",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", http.StatusAccepted),
		slog.Duration("duration", time.Since(start)),
	)
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	s.logger.Warn("method not allowed",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)
	if len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
	}
	writeErrorJSON(w, s.logger, http.StatusMethodNotAllowed,
		"method_not_allowed",
		fmt.Sprintf("method %s is not allowed on this endpoint", r.Method))
}

// buildIssueDetail assembles the live detail of one issue, or nil when no live
// entry holds it. An entry belongs to the issue when its identifier equals
// identifier, or when issueID is non-empty and the entry carries it, which
// finds an issue the tracker has since renamed.
func buildIssueDetail(identifier, issueID string, snap orchestrator.RuntimeSnapshotResult, rates TokenRates, words sentence.Context) *issueDetailResponse {
	matches := func(gotIdentifier, gotIssueID string) bool {
		return gotIdentifier == identifier || (issueID != "" && gotIssueID == issueID)
	}

	var runEntry *runningEntryResponse
	var retryEntry *retryEntryResponse
	var budgetEntry *budgetExhaustedResponse
	var parkedEntry *parkedEntryResponse
	var hopEntry *stageHopResponse

	for _, e := range snap.Running {
		if matches(e.Identifier, e.IssueID) {
			runEntry = new(toServedRunningEntry(e, snap, rates))
			runEntry.StatusText, runEntry.StatusTextFull = sentencesOf(sentence.Entry{Running: &e}, words)
			break
		}
	}

	for _, e := range snap.Retrying {
		if matches(e.Identifier, e.IssueID) {
			retryEntry = new(toRetryEntryResponse(e))
			retryEntry.StatusText, retryEntry.StatusTextFull = sentencesOf(sentence.Entry{Retry: &e}, words)
			break
		}
	}

	for _, e := range snap.BudgetExhausted {
		if matches(e.Identifier, e.IssueID) {
			budgetEntry = new(toBudgetExhaustedResponse(e))
			budgetEntry.StatusText, budgetEntry.StatusTextFull = sentencesOf(sentence.Entry{BudgetHold: &e}, words)
			break
		}
	}

	for _, e := range snap.Parked {
		if matches(e.Identifier, e.IssueID) {
			parkedEntry = new(toParkedEntryResponse(e))
			parkedEntry.StatusText, parkedEntry.StatusTextFull = sentencesOf(sentence.Entry{Parked: &e}, words)
			break
		}
	}

	for _, e := range snap.BetweenStages {
		if matches(e.Identifier, e.IssueID) {
			hopEntry = new(toStageHopResponse(e))
			hopEntry.StatusText, hopEntry.StatusTextFull = sentencesOf(sentence.Entry{BetweenStages: &e}, words)
			break
		}
	}

	var pendingRows []orchestrator.SnapshotPendingReaction
	for _, e := range snap.PendingReactions {
		if matches(e.Identifier, e.IssueID) {
			pendingRows = append(pendingRows, e)
		}
	}

	if runEntry == nil && retryEntry == nil && budgetEntry == nil && parkedEntry == nil && len(pendingRows) == 0 && hopEntry == nil {
		return nil
	}

	pending := toPendingReactionResponses(pendingRows, words)

	var status string
	var statusText, statusTextFull string
	var entryIdentifier, entryIssueID string
	var issue issueResponse

	switch {
	case runEntry != nil:
		status = "running"
		statusText, statusTextFull = runEntry.StatusText, runEntry.StatusTextFull
		entryIdentifier, entryIssueID = runEntry.IssueIdentifier, runEntry.IssueID
		issue = issueResponse{
			Title:     runEntry.IssueTitle,
			URL:       runEntry.IssueURL,
			IssueType: runEntry.IssueType,
			Labels:    runEntry.IssueLabels,
			State:     runEntry.State,
		}
	case retryEntry != nil:
		status = "retrying"
		statusText, statusTextFull = retryEntry.StatusText, retryEntry.StatusTextFull
		entryIdentifier, entryIssueID = retryEntry.IssueIdentifier, retryEntry.IssueID
		issue = issueResponse{
			Title:     retryEntry.IssueTitle,
			URL:       retryEntry.IssueURL,
			IssueType: retryEntry.IssueType,
			Labels:    retryEntry.IssueLabels,
			State:     retryEntry.State,
		}
	case budgetEntry != nil:
		status = "budget_exhausted"
		statusText, statusTextFull = budgetEntry.StatusText, budgetEntry.StatusTextFull
		entryIdentifier, entryIssueID = budgetEntry.IssueIdentifier, budgetEntry.IssueID
		issue = issueResponse{
			Title:     budgetEntry.IssueTitle,
			URL:       budgetEntry.IssueURL,
			IssueType: budgetEntry.IssueType,
			Labels:    budgetEntry.IssueLabels,
			State:     budgetEntry.State,
		}
	case parkedEntry != nil:
		status = "parked"
		statusText, statusTextFull = parkedEntry.StatusText, parkedEntry.StatusTextFull
		entryIdentifier, entryIssueID = parkedEntry.IssueIdentifier, parkedEntry.IssueID
		issue = issueResponse{
			Title:     parkedEntry.IssueTitle,
			URL:       parkedEntry.IssueURL,
			IssueType: parkedEntry.IssueType,
			Labels:    parkedEntry.IssueLabels,
			State:     parkedEntry.ParkedState,
		}
	case len(pendingRows) > 0:
		status = "pending_reactions"
		deciding := decidingPendingReaction(pending)
		statusText, statusTextFull = deciding.StatusText, deciding.StatusTextFull
		entryIdentifier, entryIssueID = pendingRows[0].Identifier, pendingRows[0].IssueID
		issue = issueResponse{
			Title:     pendingRows[0].IssueTitle,
			URL:       pendingRows[0].IssueURL,
			IssueType: pendingRows[0].IssueType,
			Labels:    pendingRows[0].IssueLabels,
			State:     pendingRows[0].State,
		}
	case hopEntry != nil:
		status = "between_stages"
		statusText, statusTextFull = hopEntry.StatusText, hopEntry.StatusTextFull
		entryIdentifier, entryIssueID = hopEntry.IssueIdentifier, hopEntry.IssueID
		issue = issueResponse{
			Title:     hopEntry.IssueTitle,
			URL:       hopEntry.IssueURL,
			IssueType: hopEntry.IssueType,
			Labels:    hopEntry.IssueLabels,
			State:     hopEntry.State,
		}
	default:
		status = "unknown"
		entryIdentifier = identifier
	}

	var ws *workspaceInfo
	if runEntry != nil && runEntry.WorkspacePath != "" {
		ws = &workspaceInfo{Path: runEntry.WorkspacePath}
	}

	var attempts *attemptsInfo
	if retryEntry != nil {
		restartCount := retryEntry.Attempt
		if restartCount > 0 {
			restartCount--
		}
		attempts = &attemptsInfo{
			RestartCount:        restartCount,
			CurrentRetryAttempt: retryEntry.Attempt,
		}
	} else {
		attempts = &attemptsInfo{}
	}

	var lastError *string
	if retryEntry != nil && retryEntry.Error != "" {
		lastError = &retryEntry.Error
	}

	return &issueDetailResponse{
		IssueIdentifier:  entryIdentifier,
		IssueID:          entryIssueID,
		Status:           status,
		Issue:            issue,
		Workspace:        ws,
		Attempts:         attempts,
		Running:          runEntry,
		Retry:            retryEntry,
		BudgetExhausted:  budgetEntry,
		Parked:           parkedEntry,
		PendingReactions: pending,
		BetweenStages:    hopEntry,
		RecentEvents:     []any{},
		LastError:        lastError,
		Tracked:          map[string]any{},
		StatusText:       statusText,
		StatusTextFull:   statusTextFull,
	}
}

// decidingPendingReaction returns the first escalated row of a non-empty
// list, else its first row: the row that decides what the issue is waiting
// on.
func decidingPendingReaction(rows []pendingReactionResponse) pendingReactionResponse {
	for _, row := range rows {
		if row.EscalatedAt != nil {
			return row
		}
	}
	return rows[0]
}

// healthResponse is the JSON wire format for GET /livez.
type healthResponse struct {
	Status string `json:"status"`
}

// readyResponse is the JSON wire format for GET /readyz.
type readyResponse struct {
	Status        string            `json:"status"`
	Version       string            `json:"version"`
	UptimeSeconds float64           `json:"uptime_seconds"`
	Checks        map[string]string `json:"checks"`
}

func (s *Server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	if s.drainingFlag.Load() {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, healthResponse{Status: "fail"})
		return
	}
	writeJSON(w, s.logger, http.StatusOK, healthResponse{Status: "pass"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	var uptime float64
	if !s.startedAt.IsZero() {
		d := max(time.Since(s.startedAt), 0)
		uptime = d.Seconds()
	}

	if s.drainingFlag.Load() {
		writeJSON(w, s.logger, http.StatusServiceUnavailable, readyResponse{
			Status:        "fail",
			Version:       s.version,
			UptimeSeconds: uptime,
			Checks: map[string]string{
				"database":  "fail",
				"preflight": "fail",
				"workflow":  "fail",
			},
		})
		return
	}

	status := "pass"
	checks := map[string]string{}

	if s.dbPingFn != nil {
		if err := s.dbPingFn(r.Context()); err != nil {
			checks["database"] = "fail"
			status = "fail"
		} else {
			checks["database"] = "pass"
		}
	} else {
		checks["database"] = "pass"
	}

	if s.preflightFn != nil && !s.preflightFn() {
		checks["preflight"] = "fail"
		status = "fail"
	} else {
		checks["preflight"] = "pass"
	}

	if s.workflowLoadedFn != nil && !s.workflowLoadedFn() {
		checks["workflow"] = "fail"
		status = "fail"
	} else {
		checks["workflow"] = "pass"
	}

	httpStatus := http.StatusOK
	if status != "pass" {
		httpStatus = http.StatusServiceUnavailable
	}

	writeJSON(w, s.logger, httpStatus, readyResponse{
		Status:        status,
		Version:       s.version,
		UptimeSeconds: uptime,
		Checks:        checks,
	})
}
