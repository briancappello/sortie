package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// dispatchBlockTTL bounds how long a dispatch block waits for the tracker
// to confirm a pull-back or cancel, so a tracker that never shows the move
// cannot hold an issue forever.
const dispatchBlockTTL = 10 * time.Minute

// planControlWriteBound bounds the tracker write a pull-back or cancel
// makes on the event loop, inside the control reply bound.
const planControlWriteBound = 4 * time.Second

// DispatchBlock keeps an issue out of dispatch after a plan pull-back or
// cancel moved it to Target, until a poll no longer reports it active or
// the block expires.
type DispatchBlock struct {
	Target  string
	Since   time.Time
	Expires time.Time
	Reason  string // "pull_back" or "cancel"
}

// clearDispatchBlocks drops each block whose issue the candidate read no
// longer reports, because the tracker then confirms it left the active
// states, and each block that expired. It runs on the event loop after
// every candidate fetch.
func clearDispatchBlocks(state *State, candidates []domain.Issue, now time.Time, log *slog.Logger) {
	if len(state.DispatchBlocks) == 0 {
		return
	}
	active := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		active[c.ID] = true
	}
	for id, b := range state.DispatchBlocks {
		switch {
		case !active[id]:
			delete(state.DispatchBlocks, id)
		case now.After(b.Expires):
			delete(state.DispatchBlocks, id)
			log.Warn("plan dispatch block expired while the tracker still reports the issue active",
				slog.String("issue_id", id),
				slog.String("target_state", b.Target),
				slog.String("reason", b.Reason),
			)
		}
	}
}

// Plan control outcomes beyond the generic ones.
const (
	// ControlAlreadyClaimed means a worker claimed the issue first.
	ControlAlreadyClaimed ControlOutcome = iota + 100
	// ControlNotReleased means the issue is not in the release state.
	ControlNotReleased
	// ControlAlreadyTerminal means the issue already reached another
	// terminal state.
	ControlAlreadyTerminal
	// ControlPlanOff means plan control is not configured.
	ControlPlanOff
	// ControlTrackerFailed means the tracker write failed.
	ControlTrackerFailed
)

// planTarget is a plan story resolved, outside the event loop, from its
// plan key.
type planTarget struct {
	IssueID    string
	Identifier string
	State      string
}

// resolvePlanKey finds the issue a plan key names through the tracker.
// It runs on the caller's goroutine, never on the event loop.
func (o *Orchestrator) resolvePlanKey(ctx context.Context, planKey string) (planTarget, ControlReply, bool) {
	cfg := o.workflowManager.Config()
	if !cfg.Tracker.PlanControl() {
		return planTarget{}, ControlReply{Outcome: ControlPlanOff, Message: "plan control is off: tracker.release_state is not set"}, false
	}
	finder, ok := o.trackerAdapter.(domain.PlanKeyFinder)
	if !ok {
		return planTarget{}, ControlReply{Outcome: ControlPlanOff, Message: "the tracker cannot find issues by plan key"}, false
	}
	pi, err := finder.FindIssueByPlanKey(ctx, planKey)
	if err != nil {
		if te, ok := errors.AsType[*domain.TrackerError](err); ok && te.Kind == domain.ErrTrackerNotFound {
			return planTarget{}, ControlReply{Outcome: ControlUnknownIssue, Message: fmt.Sprintf("no issue carries plan key %q", planKey)}, false
		}
		return planTarget{}, ControlReply{Outcome: ControlTrackerFailed, Message: err.Error()}, false
	}
	return planTarget{IssueID: pi.ID, Identifier: pi.Identifier, State: pi.State}, ControlReply{}, true
}

// PullBackFunc returns the function the HTTP server calls to pull a
// released plan story back. Safe to call from any goroutine.
func (o *Orchestrator) PullBackFunc() func(ctx context.Context, planKey string) ControlReply {
	return func(ctx context.Context, planKey string) ControlReply {
		target, reply, ok := o.resolvePlanKey(ctx, planKey)
		if !ok {
			return reply
		}
		return o.sendControl(controlRequest{Kind: controlPullBack, Identifier: target.Identifier, Plan: target})
	}
}

// PlanCancelFunc returns the function the HTTP server calls to cancel a
// plan story. Safe to call from any goroutine.
func (o *Orchestrator) PlanCancelFunc() func(ctx context.Context, planKey string) ControlReply {
	return func(ctx context.Context, planKey string) ControlReply {
		target, reply, ok := o.resolvePlanKey(ctx, planKey)
		if !ok {
			return reply
		}
		return o.sendControl(controlRequest{Kind: controlCancel, Identifier: target.Identifier, Plan: target})
	}
}

// planControlParams carries what pull-back and cancel need on the loop.
type planControlParams struct {
	Release, Backlog, Cancel string
	Terminal                 []string
	Tracker                  domain.TrackerAdapter
	Store                    retryEntryDeleter
	Metrics                  domain.Metrics
	Now                      time.Time
	Log                      *slog.Logger
}

// workerHolds reports whether a worker runs, retries, or claims the issue.
func workerHolds(state *State, issueID string) bool {
	_, running := state.Running[issueID]
	_, claimed := state.Claimed[issueID]
	_, retrying := state.RetryAttempts[issueID]
	return running || claimed || retrying
}

// applyPullBack moves a released story back to the backlog state, on the
// event loop, only while no worker holds it. The claim check and the block
// happen before the write, with no tick between them.
func applyPullBack(ctx context.Context, state *State, t planTarget, p planControlParams) ControlReply {
	log := logging.WithIssue(p.Log, t.IssueID, t.Identifier)
	if workerHolds(state, t.IssueID) {
		return ControlReply{Outcome: ControlAlreadyClaimed, Message: fmt.Sprintf("a worker already claimed %s", t.Identifier)}
	}
	if !strings.EqualFold(t.State, p.Release) {
		return ControlReply{Outcome: ControlNotReleased, Message: fmt.Sprintf("%s is in %q, not in the release state %q", t.Identifier, t.State, p.Release)}
	}
	state.DispatchBlocks[t.IssueID] = DispatchBlock{Target: p.Backlog, Since: p.Now, Expires: p.Now.Add(dispatchBlockTTL), Reason: "pull_back"}
	wctx, cancel := context.WithTimeout(ctx, planControlWriteBound)
	defer cancel()
	if err := p.Tracker.TransitionIssue(wctx, t.IssueID, p.Backlog); err != nil {
		delete(state.DispatchBlocks, t.IssueID)
		log.Warn("plan pull-back failed", slog.Any("error", err))
		return ControlReply{Outcome: ControlTrackerFailed, Message: err.Error()}
	}
	log.Info("plan story pulled back", slog.String("state", p.Backlog))
	return ControlReply{Outcome: ControlDone}
}

// applyCancel stops a plan story's worker, if one runs, and moves the issue
// to the cancel state, on the event loop. The cancel state is terminal, so
// neither reconcile nor retry starts the issue again.
func applyCancel(ctx context.Context, state *State, t planTarget, p planControlParams) ControlReply {
	log := logging.WithIssue(p.Log, t.IssueID, t.Identifier)
	if strings.EqualFold(t.State, p.Cancel) {
		return ControlReply{Outcome: ControlDone, Message: "already cancelled"}
	}
	if _, terminal := stateSet(p.Terminal)[strings.ToLower(t.State)]; terminal {
		return ControlReply{Outcome: ControlAlreadyTerminal, Message: fmt.Sprintf("%s is already in the terminal state %q", t.Identifier, t.State)}
	}
	if entry, running := state.Running[t.IssueID]; running {
		if !entry.PendingCleanup {
			if entry.CancelFunc != nil {
				entry.CancelFunc()
			}
			endHoldRecorded(entry, p.Now, p.Metrics)
			entry.PendingCleanup = true
			entry.ObservedTerminalState = p.Cancel
		}
		log.Info("stopping worker for cancelled plan story")
	}
	releaseIssueRuntimeState(ctx, state, p.Store, t.IssueID, log)
	state.DispatchBlocks[t.IssueID] = DispatchBlock{Target: p.Cancel, Since: p.Now, Expires: p.Now.Add(dispatchBlockTTL), Reason: "cancel"}

	wctx, cancel := context.WithTimeout(ctx, planControlWriteBound)
	defer cancel()
	if err := p.Tracker.TransitionIssue(wctx, t.IssueID, p.Cancel); err != nil {
		// The worker is stopped either way. The block stays, so the issue
		// is not dispatched again before the cancel is retried.
		log.Warn("plan cancel could not move the issue", slog.Any("error", err))
		return ControlReply{Outcome: ControlTrackerFailed, Message: err.Error()}
	}
	log.Info("plan story cancelled", slog.String("state", p.Cancel))
	return ControlReply{Outcome: ControlDone}
}
