package orchestrator

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// ControlOutcome is the event loop's verdict on a control request.
type ControlOutcome int

const (
	// ControlDone means the request was applied, or had nothing to change.
	ControlDone ControlOutcome = iota

	// ControlAccepted means the request was accepted for delivery.
	ControlAccepted

	// ControlUnknownIssue means no issue with the identifier is known.
	ControlUnknownIssue

	// ControlNotRunning means the issue is known but has no running worker.
	ControlNotRunning

	// ControlInvalid means the request itself is malformed.
	ControlInvalid

	// ControlRateLimited means the request came too soon after the last
	// one of its kind for the same issue.
	ControlRateLimited

	// ControlUnavailable means the event loop did not answer, because it is
	// shutting down or busy past the reply bound.
	ControlUnavailable
)

// HoldState is a run's hold as the control routes report it.
type HoldState struct {
	Identifier string
	IssueID    string
	Held       bool
	Reason     string
	Holder     string
	HeldSince  time.Time
	HeldTotal  time.Duration
}

// ControlReply is the event loop's answer to a control request.
type ControlReply struct {
	Outcome ControlOutcome
	Hold    HoldState
	Message string
}

type controlKind int

const (
	controlHold controlKind = iota
	controlRelease
)

// controlRequest asks the event loop to change one issue's run.
type controlRequest struct {
	Kind       controlKind
	Identifier string
	Reason     string
	Holder     string
	ReplyCh    chan ControlReply
}

// controlReplyBound bounds how long a control call waits for the event
// loop, both to accept the request and to answer it.
const controlReplyBound = 5 * time.Second

// findIssue resolves identifier to the running entry serving it, and
// reports whether the identifier names any issue the runtime state
// knows: running, waiting to retry, or held out by a budget ceiling.
func findIssue(state *State, identifier string) (issueID string, entry *RunningEntry, known bool) {
	for id, e := range state.Running {
		if e.Identifier == identifier {
			return id, e, true
		}
	}
	for id, r := range state.RetryAttempts {
		if r.Identifier == identifier {
			return id, nil, true
		}
	}
	for id, b := range state.BudgetExhausted {
		if b.Identifier == identifier {
			return id, nil, true
		}
	}
	return "", nil, false
}

// holdStateOf returns the hold view of a running entry.
func holdStateOf(issueID string, entry *RunningEntry, now time.Time) HoldState {
	total := entry.HeldTotal
	if !entry.HeldSince.IsZero() {
		total += now.Sub(entry.HeldSince)
	}
	return HoldState{
		Identifier: entry.Identifier,
		IssueID:    issueID,
		Held:       !entry.HeldSince.IsZero(),
		Reason:     entry.HoldReason,
		Holder:     entry.Holder,
		HeldSince:  entry.HeldSince,
		HeldTotal:  total,
	}
}

// applyHold holds the running entry for identifier at now. A hold already
// in effect keeps its start, reason, and holder.
func applyHold(state *State, identifier, reason, holder string, now time.Time, log *slog.Logger) ControlReply {
	issueID, entry, known := findIssue(state, identifier)
	switch {
	case !known:
		return ControlReply{Outcome: ControlUnknownIssue, Message: fmt.Sprintf("issue %q is not known", identifier)}
	case entry == nil:
		return ControlReply{Outcome: ControlNotRunning, Message: fmt.Sprintf("issue %q has no running worker", identifier)}
	}
	if entry.HeldSince.IsZero() {
		entry.HeldSince = now
		entry.HoldReason = reason
		entry.Holder = holder
		entry.TurnClock.hold(now)
		logging.WithIssue(log, issueID, entry.Identifier).Info("run held",
			slog.String("hold_reason", reason),
			slog.String("holder", holder),
		)
	}
	return ControlReply{Outcome: ControlDone, Hold: holdStateOf(issueID, entry, now)}
}

// applyRelease ends the hold of the running entry for identifier at now.
// Releasing a run that is not held changes nothing.
func applyRelease(state *State, identifier string, now time.Time, metrics domain.Metrics, log *slog.Logger) ControlReply {
	issueID, entry, known := findIssue(state, identifier)
	switch {
	case !known:
		return ControlReply{Outcome: ControlUnknownIssue, Message: fmt.Sprintf("issue %q is not known", identifier)}
	case entry == nil:
		return ControlReply{Outcome: ControlNotRunning, Message: fmt.Sprintf("issue %q has no running worker", identifier)}
	}
	ended := endHoldRecorded(entry, now, metrics)
	if ended > 0 {
		logging.WithIssue(log, issueID, entry.Identifier).Info("run released",
			slog.Int64("held_ms", ended.Milliseconds()),
			slog.Int64("held_total_ms", entry.HeldTotal.Milliseconds()),
		)
	}
	return ControlReply{Outcome: ControlDone, Hold: holdStateOf(issueID, entry, now)}
}

// endHoldRecorded ends the entry's hold at now, as [endHold] does, and
// counts the hold it ended toward the run's rule in metrics.
func endHoldRecorded(entry *RunningEntry, now time.Time, metrics domain.Metrics) time.Duration {
	ended := endHold(entry, now)
	if ended > 0 && metrics != nil {
		metrics.AddHoldSeconds(normalizeDispatchRuleName(entry.RuleName), ended.Seconds())
	}
	return ended
}

// endHold ends the entry's hold at now, if one is in effect: it adds the
// hold to the run's total, restarts the stall clock at now, and credits
// the turn clock. It returns the duration of the hold it ended.
func endHold(entry *RunningEntry, now time.Time) time.Duration {
	if entry.HeldSince.IsZero() {
		return 0
	}
	ended := max(now.Sub(entry.HeldSince), 0)
	entry.HeldTotal += ended
	entry.HeldSince = time.Time{}
	entry.HoldReason = ""
	entry.Holder = ""
	entry.HoldReleasedAt = now
	entry.TurnClock.release(now)
	return ended
}

// heldRuns counts the running entries currently held.
func heldRuns(state *State) int {
	n := 0
	for _, e := range state.Running {
		if !e.HeldSince.IsZero() {
			n++
		}
	}
	return n
}
