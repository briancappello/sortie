package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

type nopDeleter struct{ deleted []string }

func (d *nopDeleter) DeleteRetryEntry(_ context.Context, id string) error {
	d.deleted = append(d.deleted, id)
	return nil
}

func planParams(tr domain.TrackerAdapter) planControlParams {
	return planControlParams{
		Release: "Todo", Backlog: "Backlog", Cancel: "Canceled",
		Terminal: []string{"Done", "Canceled"},
		Tracker:  tr, Store: &nopDeleter{}, Metrics: &domain.NoopMetrics{},
		Now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Log: discardLogger(),
	}
}

var invoice = planTarget{IssueID: "i-inv", Identifier: "HULY-5", State: "Todo"}

func todoCandidate(id string) domain.Issue {
	return domain.Issue{ID: id, Identifier: "HULY-5", Title: "Invoice", State: "Todo"}
}

func TestPullBack_BeforeDispatch(t *testing.T) {
	t.Parallel()
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	tr := &mgcTrackerFake{}
	reply := applyPullBack(context.Background(), st, invoice, planParams(tr))

	if reply.Outcome != ControlDone {
		t.Fatalf("reply = %+v, want done", reply)
	}
	if len(tr.transitionCalls) != 1 || tr.transitionCalls[0].target != "Backlog" {
		t.Errorf("transitions = %+v, want one to Backlog", tr.transitionCalls)
	}
	// The next poll still reads Todo from a stale read: no dispatch.
	d := EvaluateCandidate(context.Background(), todoCandidate("i-inv"), st, stateSet([]string{"Todo"}), planTerminal, nil, &TickResolution{})
	if d.Dispatch {
		t.Error("dispatched from a stale snapshot after a pull-back")
	}
}

// A worker that claimed the issue in the same tick wins.
func TestPullBack_LosesToAClaim(t *testing.T) {
	t.Parallel()
	for name, mark := range map[string]func(*State){
		"claimed":  func(s *State) { s.Claimed["i-inv"] = struct{}{} },
		"running":  func(s *State) { s.Running["i-inv"] = &RunningEntry{Identifier: "HULY-5"} },
		"retrying": func(s *State) { s.RetryAttempts["i-inv"] = &RetryEntry{IssueID: "i-inv"} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := NewState(1000, 2, 0, nil, AgentTotals{})
			mark(st)
			tr := &mgcTrackerFake{}
			reply := applyPullBack(context.Background(), st, invoice, planParams(tr))
			if reply.Outcome != ControlAlreadyClaimed {
				t.Errorf("reply = %+v, want already_claimed", reply)
			}
			if len(tr.transitionCalls) != 0 || len(st.DispatchBlocks) != 0 {
				t.Error("pull-back changed something after losing to a claim")
			}
		})
	}
}

func TestPullBack_NotReleased(t *testing.T) {
	t.Parallel()
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	target := invoice
	target.State = "Backlog"
	if reply := applyPullBack(context.Background(), st, target, planParams(&mgcTrackerFake{})); reply.Outcome != ControlNotReleased {
		t.Errorf("reply = %+v, want not_released", reply)
	}
}

// A failed write leaves no block behind.
func TestPullBack_FailedWriteLeavesNoBlock(t *testing.T) {
	t.Parallel()
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	tr := &mgcTrackerFake{transitionErr: errors.New("huly down")}
	if reply := applyPullBack(context.Background(), st, invoice, planParams(tr)); reply.Outcome != ControlTrackerFailed {
		t.Errorf("reply = %+v, want tracker failure", reply)
	}
	if len(st.DispatchBlocks) != 0 {
		t.Error("block left after a failed pull-back")
	}
}

func TestDispatchBlocks_ClearAndExpire(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	st.DispatchBlocks["gone"] = DispatchBlock{Target: "Backlog", Expires: now.Add(time.Minute)}
	st.DispatchBlocks["stale"] = DispatchBlock{Target: "Backlog", Expires: now.Add(time.Minute)}
	st.DispatchBlocks["expired"] = DispatchBlock{Target: "Backlog", Expires: now.Add(-time.Second)}

	clearDispatchBlocks(st, []domain.Issue{{ID: "stale"}, {ID: "expired"}}, now, discardLogger())

	if _, ok := st.DispatchBlocks["gone"]; ok {
		t.Error("block kept for an issue the poll no longer reports active")
	}
	if _, ok := st.DispatchBlocks["stale"]; !ok {
		t.Error("block cleared while a stale read still shows the issue active")
	}
	if _, ok := st.DispatchBlocks["expired"]; ok {
		t.Error("expired block kept")
	}
}

func TestCancel_RunningWorker(t *testing.T) {
	t.Parallel()
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	cancelled := false
	st.Running["i-inv"] = &RunningEntry{Identifier: "HULY-5", CancelFunc: func() { cancelled = true }}
	st.Claimed["i-inv"] = struct{}{}
	st.RetryAttempts["i-inv"] = &RetryEntry{IssueID: "i-inv"}
	tr := &mgcTrackerFake{}
	target := invoice
	target.State = "In Progress"

	reply := applyCancel(context.Background(), st, target, planParams(tr))

	if reply.Outcome != ControlDone || !cancelled {
		t.Fatalf("reply = %+v, cancelled = %v, want done and the worker cancelled", reply, cancelled)
	}
	entry := st.Running["i-inv"]
	if !entry.PendingCleanup || entry.ObservedTerminalState != "Canceled" {
		t.Errorf("entry = %+v, want cleanup pending for Canceled", entry)
	}
	if _, ok := st.RetryAttempts["i-inv"]; ok {
		t.Error("retry kept after a cancel")
	}
	if _, ok := st.Claimed["i-inv"]; ok {
		t.Error("claim kept after a cancel")
	}
	if len(tr.transitionCalls) != 1 || tr.transitionCalls[0].target != "Canceled" {
		t.Errorf("transitions = %+v, want one to Canceled", tr.transitionCalls)
	}
}

func TestCancel_Outcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state string
		want  ControlOutcome
		write bool
	}{
		{"Backlog", ControlDone, true},
		{"Canceled", ControlDone, false},
		{"canceled", ControlDone, false},
		{"Done", ControlAlreadyTerminal, false},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			t.Parallel()
			st := NewState(1000, 2, 0, nil, AgentTotals{})
			tr := &mgcTrackerFake{}
			target := invoice
			target.State = tt.state
			reply := applyCancel(context.Background(), st, target, planParams(tr))
			if reply.Outcome != tt.want || (len(tr.transitionCalls) == 1) != tt.write {
				t.Errorf("reply = %+v, writes = %d, want %v with write=%v", reply, len(tr.transitionCalls), tt.want, tt.write)
			}
		})
	}
}

// A failed cancel write keeps the block, so the stopped story is not
// dispatched again before the cancel is retried.
func TestCancel_FailedWriteKeepsTheBlock(t *testing.T) {
	t.Parallel()
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	tr := &mgcTrackerFake{transitionErr: errors.New("huly down")}
	if reply := applyCancel(context.Background(), st, invoice, planParams(tr)); reply.Outcome != ControlTrackerFailed {
		t.Errorf("reply = %+v, want tracker failure", reply)
	}
	if _, ok := st.DispatchBlocks["i-inv"]; !ok {
		t.Error("block dropped after a failed cancel write")
	}
}
