package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/parkanswer"
)

// parkAnswerWriteBound bounds the tracker comment an answer posts on the
// event loop, inside the control reply bound.
const parkAnswerWriteBound = 4 * time.Second

// ParkAnswer is what an answered park reports back to the caller.
type ParkAnswer struct {
	IssueID    string
	Identifier string
	Author     string
	Reason     string
	Statement  string
	ParkedAt   time.Time
	ReleasedAt time.Time
}

// parkAnswerParams carries what [applyAnswer] needs besides the request.
type parkAnswerParams struct {
	Tracker domain.TrackerAdapter
	Store   ParkStore
	Log     *slog.Logger
}

// findParked returns the issue ID and park of the parked issue whose
// identifier is identifier.
func findParked(state *State, identifier string) (string, *ParkedEntry, bool) {
	for id, e := range state.Parked {
		if e.Identifier == identifier {
			return id, e, true
		}
	}
	return "", nil, false
}

// applyAnswer posts a person's answer as a comment on the parked issue named
// by identifier and then releases the park the way a person's gesture in the
// tracker does, so the issue's next dispatch hands the run the statement and
// the comments made since the park, this one among them. An issue that is
// not parked gets [ControlNotParked] and nothing is posted. A failed comment
// leaves the park in place.
func applyAnswer(ctx context.Context, state *State, identifier string, answer parkanswer.Answer, p parkAnswerParams) ControlReply {
	issueID, entry, parked := findParked(state, identifier)
	if parked && p.Tracker == nil {
		return ControlReply{Outcome: ControlUnavailable, Message: "the tracker is not available"}
	}
	wctx, cancel := context.WithTimeout(ctx, parkAnswerWriteBound)
	defer cancel()
	posted, err := parkanswer.Post(wctx, p.Tracker, issueID, parked, answer)
	switch {
	case errors.Is(err, parkanswer.ErrInvalid):
		return ControlReply{Outcome: ControlInvalid, Message: err.Error()}
	case errors.Is(err, parkanswer.ErrNotParked):
		return ControlReply{Outcome: ControlNotParked, Message: fmt.Sprintf("issue %q is not parked", identifier)}
	}
	log := logging.WithIssue(p.Log, issueID, entry.Identifier)
	if err != nil {
		log.Warn("park answer comment failed", slog.Any("error", err))
		return ControlReply{Outcome: ControlTrackerFailed, Message: err.Error()}
	}
	log.Info("park answered", slog.String("author", posted.Author))
	unparkIssue(ctx, state, issueID, unparkTriggerAnswered, p.Store, log)
	return ControlReply{Outcome: ControlDone, Answer: ParkAnswer{
		IssueID:    issueID,
		Identifier: entry.Identifier,
		Author:     posted.Author,
		Reason:     entry.Reason,
		Statement:  entry.Statement,
		ParkedAt:   entry.ParkedAt,
		ReleasedAt: entry.ReleasedAt,
	}}
}

// AnswerFunc returns the function the HTTP server calls to answer the
// parked issue named by identifier. An answered park queues a poll so the
// issue's next run starts without waiting for the poll interval. Safe to
// call from any goroutine.
func (o *Orchestrator) AnswerFunc() func(identifier string, answer parkanswer.Answer) ControlReply {
	refresh := o.RefreshFunc()
	return func(identifier string, answer parkanswer.Answer) ControlReply {
		reply := o.sendControl(controlRequest{Kind: controlAnswer, Identifier: identifier, Answer: answer})
		if reply.Outcome == ControlDone {
			refresh()
		}
		return reply
	}
}
