package sentence

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// AttentionItem is one thing that needs a person, worded and ready to serve.
// Kind is parked, budget_held, reaction_escalated, workflow_reload_failed, or
// preflight_failed; Severity is critical or warning. Since is nil only for a
// park whose stored time did not parse. The issue fields are empty on a
// process-wide item.
type AttentionItem struct {
	ID                string
	Kind              string
	Severity          string
	Since             *time.Time
	IssueID           string
	IssueIdentifier   string
	DisplayIdentifier string
	IssueTitle        string
	IssueURL          string
	ReasonText        string
	WhyText           string
	ClearsText        string
	Action            AttentionAction
	Evidence          AttentionEvidence
}

// AttentionAction is the one thing a client offers for an item. Kind is
// open_issue, open_pull_request, or acknowledge; URL is set exactly for the
// first two.
type AttentionAction struct {
	Kind string
	URL  string
}

// AttentionEvidence holds the facts behind an item. Each member is set on the
// kinds that carry it and left zero on every other.
type AttentionEvidence struct {
	Reason             string
	Label              string
	UsedSessions       *int
	BudgetSessions     *int
	UsedTokens         *int64
	UsedTokensComplete *bool
	BudgetTokens       *int64
	ReactionKind       string
	ConfigKey          string
	PullRequest        *orchestrator.SnapshotPullRequest
	EscalationTrigger  string
	UsedAttempts       *int
	MaxAttempts        *int
	LastCheck          *orchestrator.ReactionCheck
	File               string
	Error              string
}

const (
	kindParked               = "parked"
	kindBudgetHeld           = "budget_held"
	kindReactionEscalated    = "reaction_escalated"
	kindWorkflowReloadFailed = "workflow_reload_failed"
	kindPreflightFailed      = "preflight_failed"
)

const (
	severityCritical = "critical"
	severityWarning  = "warning"
)

const (
	actionOpenIssue       = "open_issue"
	actionOpenPullRequest = "open_pull_request"
	actionAcknowledge     = "acknowledge"
)

const (
	parkReasonHandoffAbsence = "handoff_absence"
	budgetReasonTokens       = "token_budget"
	checkVerdictFailing      = "failing"
	checkVerdictPending      = "pending"
)

// escalatedTemplates names the attention template of each escalating reaction
// kind. A kind it lacks renders the fallback text and keeps its item.
var escalatedTemplates = map[string]string{
	orchestrator.ReactionKindCI:              "attn_escalated_ci",
	orchestrator.ReactionKindReview:          "attn_escalated_review",
	orchestrator.ReactionKindBotReview:       "attn_escalated_bot_review",
	orchestrator.ReactionKindMergeConflict:   "attn_escalated_merge_conflict",
	orchestrator.ReactionKindAutoMerge:       "attn_escalated_merge",
	orchestrator.ReactionKindMergeCompletion: "attn_escalated_merge_completion",
}

// Attention returns the items that need a person, ordered by severity, then
// since with unknown times first, then id; the list is never nil. A nil health
// leaves out the process-wide items. It reads only its arguments and is safe
// for concurrent use.
func Attention(snap orchestrator.RuntimeSnapshotResult, health *orchestrator.SnapshotHealth, c Context) []AttentionItem {
	items := make([]AttentionItem, 0, len(snap.Parked)+len(snap.BudgetExhausted)+len(snap.PendingReactions)+2)
	for _, e := range snap.Parked {
		items = append(items, parkedItem(e))
	}
	for _, e := range snap.BudgetExhausted {
		items = append(items, budgetItem(e))
	}
	for _, e := range snap.PendingReactions {
		if e.EscalatedAt != nil {
			items = append(items, escalatedReactionItem(e, c))
		}
	}
	if health != nil {
		if health.Workflow.ConsecutiveFailures > 0 {
			items = append(items, workflowReloadItem(health.Workflow))
		}
		if health.Preflight.ConsecutiveFailures > 0 && !health.Preflight.WorkflowLoadFailed {
			items = append(items, preflightItem(health.Preflight.SnapshotHealthCheck))
		}
	}
	slices.SortFunc(items, compareAttention)
	return items
}

func parkedItem(e orchestrator.SnapshotParkedEntry) AttentionItem {
	template := "attn_parked_agent_blocked"
	if e.Reason == parkReasonHandoffAbsence {
		template = "attn_parked_handoff_absence"
	}
	reason, why, clears := attentionTexts(template, map[string]any{
		"Key":    issueKey(e.DisplayID, e.Identifier),
		"Label":  strings.TrimSpace(e.Label),
		"Unseen": !e.LabelApplied,
		"State":  strings.TrimSpace(e.ParkedState),
	})

	// A restart restores the park at whole seconds, so both sides of it must
	// hash and report the same precision.
	var since *time.Time
	var parkedAt string
	if !e.ParkedAt.IsZero() {
		since = new(e.ParkedAt.UTC().Truncate(time.Second))
		parkedAt = since.Format(time.RFC3339)
	}
	return AttentionItem{
		ID:                attentionID(kindParked, e.IssueID, parkedAt),
		Kind:              kindParked,
		Severity:          severityWarning,
		Since:             since,
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		ReasonText:        reason,
		WhyText:           why,
		ClearsText:        clears,
		Action:            issueAction(e.IssueURL),
		Evidence:          AttentionEvidence{Reason: e.Reason, Label: e.Label},
	}
}

func budgetItem(e orchestrator.SnapshotBudgetEntry) AttentionItem {
	key := issueKey(e.DisplayID, e.Identifier)
	evidence := AttentionEvidence{Reason: e.Reason}
	var template, ceiling string
	var data map[string]any
	if e.Reason == budgetReasonTokens {
		used, atLeast := heldTokenSpend(&e)
		stopped := 0
		if e.StoppedInFlight != nil {
			stopped = *e.StoppedInFlight
		}
		template, ceiling = "attn_budget_tokens", strconv.FormatInt(e.BudgetTokens, 10)
		data = map[string]any{
			"Key":     key,
			"Used":    used,
			"AtLeast": atLeast,
			"Budget":  new(e.BudgetTokens),
			"Stopped": stopped,
		}
		evidence.UsedTokens = e.UsedTokens
		evidence.UsedTokensComplete = new(!atLeast)
		evidence.BudgetTokens = new(e.BudgetTokens)
	} else {
		template, ceiling = "attn_budget_sessions", strconv.Itoa(e.BudgetSessions)
		data = map[string]any{
			"Key":            key,
			"UsedSessions":   e.UsedSessions,
			"BudgetSessions": e.BudgetSessions,
		}
		evidence.UsedSessions = new(e.UsedSessions)
		evidence.BudgetSessions = new(e.BudgetSessions)
	}
	reason, why, clears := attentionTexts(template, data)
	return AttentionItem{
		ID:                attentionID(kindBudgetHeld, e.IssueID, e.Reason, ceiling),
		Kind:              kindBudgetHeld,
		Severity:          severityWarning,
		Since:             new(e.ExhaustedAt.UTC()),
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		ReasonText:        reason,
		WhyText:           why,
		ClearsText:        clears,
		Action:            AttentionAction{Kind: actionAcknowledge},
		Evidence:          evidence,
	}
}

func escalatedReactionItem(e orchestrator.SnapshotPendingReaction, c Context) AttentionItem {
	data := map[string]any{
		"Key":           issueKey(e.DisplayID, e.Identifier),
		"PR":            e.PullRequest.Number,
		"Trigger":       e.EscalationTrigger,
		"Tries":         0,
		"HasMax":        false,
		"Max":           0,
		"Latest":        "",
		"FailingChecks": []string{},
		"FailingN":      0,
		"Handoff":       "",
		"Target":        "",
	}
	if e.UsedAttempts != nil {
		data["Tries"] = *e.UsedAttempts
	}
	if e.MaxAttempts != nil {
		data["HasMax"] = true
		data["Max"] = *e.MaxAttempts
	}
	if ci := checkedCI(e.LastCheck); ci != nil {
		if ci.Status == checkVerdictFailing || ci.Status == checkVerdictPending {
			data["Latest"] = ci.Status
		}
		data["FailingChecks"] = trimmed(ci.FailingChecks)
		data["FailingN"] = ci.Failing
	}
	addShapeFacts(data, e.Kind, c.Shape)

	reason, why, clears := attentionTexts(escalatedTemplates[e.Kind], data)
	pullRequest := e.PullRequest
	return AttentionItem{
		ID:                attentionID(kindReactionEscalated, e.IssueID, e.Kind, e.EscalatedAt.UTC().Format(time.RFC3339Nano)),
		Kind:              kindReactionEscalated,
		Severity:          severityWarning,
		Since:             new(e.EscalatedAt.UTC()),
		IssueID:           e.IssueID,
		IssueIdentifier:   e.Identifier,
		DisplayIdentifier: e.DisplayID,
		IssueTitle:        e.IssueTitle,
		IssueURL:          e.IssueURL,
		ReasonText:        reason,
		WhyText:           why,
		ClearsText:        clears,
		Action:            escalationAction(e),
		Evidence: AttentionEvidence{
			ReactionKind:      e.Kind,
			ConfigKey:         e.ConfigKey,
			PullRequest:       &pullRequest,
			EscalationTrigger: e.EscalationTrigger,
			UsedAttempts:      e.UsedAttempts,
			MaxAttempts:       e.MaxAttempts,
			LastCheck:         e.LastCheck,
		},
	}
}

func workflowReloadItem(w orchestrator.SnapshotWorkflowHealth) AttentionItem {
	return failingCheckItem(kindWorkflowReloadFailed, "attn_reload", w.SnapshotHealthCheck,
		map[string]any{"File": w.File, "Error": strings.TrimSpace(w.Error)},
		AttentionEvidence{File: w.File, Error: w.Error})
}

func preflightItem(check orchestrator.SnapshotHealthCheck) AttentionItem {
	leadless := strings.TrimSpace(strings.TrimPrefix(check.Error, orchestrator.PreflightFailedPrefix))
	return failingCheckItem(kindPreflightFailed, "attn_preflight", check,
		map[string]any{"Error": leadless},
		AttentionEvidence{Error: check.Error})
}

// failingCheckItem builds a process-wide item, which no issue owns and only an
// acknowledgement answers.
func failingCheckItem(kind, template string, check orchestrator.SnapshotHealthCheck, data map[string]any, evidence AttentionEvidence) AttentionItem {
	failingSince := check.FailingSince.UTC()
	reason, why, clears := attentionTexts(template, data)
	return AttentionItem{
		ID:         attentionID(kind, failingSince.Format(time.RFC3339Nano)),
		Kind:       kind,
		Severity:   severityCritical,
		Since:      &failingSince,
		ReasonText: reason,
		WhyText:    why,
		ClearsText: clears,
		Action:     AttentionAction{Kind: actionAcknowledge},
		Evidence:   evidence,
	}
}

// attentionTexts renders the three texts of an attention template. A part that
// fails to render reads as the fallback text, so the item stays.
func attentionTexts(template string, data map[string]any) (reason, why, clears string) {
	return render(template, "reason", data), render(template, "why", data), render(template, "clears", data)
}

// issueAction opens the issue when it has a URL and otherwise asks for an
// acknowledgement.
func issueAction(issueURL string) AttentionAction {
	if issueURL == "" {
		return AttentionAction{Kind: actionAcknowledge}
	}
	return AttentionAction{Kind: actionOpenIssue, URL: issueURL}
}

// escalationAction opens the pull request an escalation names, except for
// merge completion, whose pull request has merged and whose open work is the
// issue.
func escalationAction(e orchestrator.SnapshotPendingReaction) AttentionAction {
	if e.Kind != orchestrator.ReactionKindMergeCompletion && e.PullRequest.URL != "" {
		return AttentionAction{Kind: actionOpenPullRequest, URL: e.PullRequest.URL}
	}
	return issueAction(e.IssueURL)
}

// attentionID hashes the kind and its identity parts into the item id. The
// encoding is part of the contract: clients store acknowledgements by id.
func attentionID(kind string, parts ...string) string {
	encoded, _ := json.Marshal(append([]string{kind}, parts...))
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:16])
}

func severityRank(severity string) int {
	if severity == severityCritical {
		return 0
	}
	return 1
}

func compareAttention(a, b AttentionItem) int {
	return cmp.Or(
		cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)),
		compareSince(a.Since, b.Since),
		cmp.Compare(a.ID, b.ID),
	)
}

// compareSince orders an unknown time before every known one.
func compareSince(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.Compare(*b)
}
