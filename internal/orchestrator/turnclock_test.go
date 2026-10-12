package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeNow) set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t
}

func fakeClock(t0 time.Time) (*turnClock, *fakeNow) {
	f := &fakeNow{t: t0}
	c := newTurnClock()
	c.now = f.now
	return c, f
}

// eventually polls cond for up to a second.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestTurnClock_HoldExactlyAtTheDeadline(t *testing.T) {
	t.Parallel()

	t0 := reconcileBaseTime
	clock, now := fakeClock(t0)
	var expired atomic.Bool
	stop := clock.watch(context.Background(), time.Hour, func() { expired.Store(true) })
	defer stop()

	// The hold arrives at the very instant the deadline is reached.
	now.set(t0.Add(time.Hour))
	clock.hold(t0.Add(time.Hour))
	time.Sleep(50 * time.Millisecond)
	if expired.Load() {
		t.Fatal("turn expired although the hold began exactly at the deadline")
	}

	// Releasing ten minutes later moves the deadline by those ten minutes,
	// which is the release instant itself.
	now.set(t0.Add(70 * time.Minute))
	clock.release(t0.Add(70 * time.Minute))
	if deadline, held, _ := clock.deadline(); held || !deadline.Equal(t0.Add(70*time.Minute)) {
		t.Errorf("deadline after release = %v (held %v), want %v", deadline, held, t0.Add(70*time.Minute))
	}
	if !eventually(expired.Load) {
		t.Error("turn did not expire once its extended deadline had passed")
	}
}

func TestTurnClock_ExtendedDeadlineAfterRelease(t *testing.T) {
	t.Parallel()

	t0 := reconcileBaseTime
	clock, now := fakeClock(t0)
	var expired atomic.Bool
	stop := clock.watch(context.Background(), 30*time.Minute, func() { expired.Store(true) })
	defer stop()

	now.set(t0.Add(5 * time.Minute))
	clock.hold(t0.Add(5 * time.Minute))
	now.set(t0.Add(15 * time.Minute))
	clock.release(t0.Add(15 * time.Minute))

	deadline, held, _ := clock.deadline()
	if held || !deadline.Equal(t0.Add(40*time.Minute)) {
		t.Errorf("deadline = %v (held %v), want turn start plus 30m plus the 10m held = %v", deadline, held, t0.Add(40*time.Minute))
	}
	time.Sleep(20 * time.Millisecond)
	if expired.Load() {
		t.Error("turn expired before its extended deadline")
	}
}

func TestTurnClock_HoldBeforeTheTurnCountsOnlyFromTurnStart(t *testing.T) {
	t.Parallel()

	t0 := reconcileBaseTime
	clock, now := fakeClock(t0)
	clock.hold(t0.Add(-time.Hour))
	stop := clock.watch(context.Background(), 30*time.Minute, func() {})
	defer stop()

	now.set(t0.Add(10 * time.Minute))
	clock.release(t0.Add(10 * time.Minute))
	if deadline, _, _ := clock.deadline(); !deadline.Equal(t0.Add(40 * time.Minute)) {
		t.Errorf("deadline = %v, want %v: only the held time inside the turn extends it", deadline, t0.Add(40*time.Minute))
	}
}

// blockingTurnAdapter runs a turn until its context ends and reports the
// cancellation the way adapters do.
func blockingTurnAdapter(ended chan<- time.Time) *mockAgentAdapter {
	return &mockAgentAdapter{runTurnFn: func(ctx context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
		<-ctx.Done()
		ended <- time.Now()
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCancelled},
			&domain.AgentError{Kind: domain.ErrTurnCancelled, Message: "turn cancelled", Err: ctx.Err()}
	}}
}

func TestRunBoundedTurn_HoldExtendsTheTurn(t *testing.T) {
	t.Parallel()

	const bound = 300 * time.Millisecond
	tests := []struct {
		name               string
		holdAt, releaseAt  time.Duration // relative to the turn start; holdAt < 0 holds before it
		wantNotBefore      time.Duration
		wantTimeoutOutcome bool
	}{
		{"no hold", 0, 0, bound, true},
		{"hold before the turn", -1, 200 * time.Millisecond, bound + 200*time.Millisecond, true},
		{"hold during the turn", 100 * time.Millisecond, 400 * time.Millisecond, bound + 300*time.Millisecond, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := newTurnClock()
			ctx := withSessionIdentity(context.Background(), sessionIdentity{TurnClock: clock})
			ended := make(chan time.Time, 1)
			adapter := blockingTurnAdapter(ended)

			if tt.holdAt < 0 {
				clock.hold(time.Now())
			}
			start := time.Now()
			if tt.releaseAt > 0 {
				go func() {
					if tt.holdAt > 0 {
						time.Sleep(tt.holdAt)
						clock.hold(time.Now())
					}
					time.Sleep(time.Until(start.Add(tt.releaseAt)))
					clock.release(time.Now())
				}()
			}

			_, err := runBoundedTurn(ctx, adapter, domain.Session{ID: "s"}, domain.RunTurnParams{OnEvent: func(domain.AgentEvent) {}}, int(bound.Milliseconds()), discardLogger())

			// The test stamps its hold a little after it intends to, which
			// shortens the measured hold by that much.
			const slack = 25 * time.Millisecond
			elapsed := (<-ended).Sub(start)
			if elapsed < tt.wantNotBefore-slack {
				t.Errorf("turn ended after %v, want no earlier than %v", elapsed, tt.wantNotBefore)
			}
			if elapsed > tt.wantNotBefore+2*time.Second {
				t.Errorf("turn ended after %v, want shortly after %v", elapsed, tt.wantNotBefore)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnTimeout {
				t.Errorf("runBoundedTurn() error = %v, want %s", err, domain.ErrTurnTimeout)
			}
		})
	}
}

func TestRunBoundedTurn_HeldTurnIsNeverEnded(t *testing.T) {
	t.Parallel()

	clock := newTurnClock()
	clock.hold(time.Now())
	parent, cancel := context.WithCancel(context.Background())
	ctx := withSessionIdentity(parent, sessionIdentity{TurnClock: clock})
	ended := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		_, err := runBoundedTurn(ctx, blockingTurnAdapter(ended), domain.Session{ID: "s"}, domain.RunTurnParams{OnEvent: func(domain.AgentEvent) {}}, 50, discardLogger())
		done <- err
	}()

	select {
	case <-ended:
		t.Fatal("a held turn ended at its bound")
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	if err := <-done; err == nil {
		t.Error("runBoundedTurn() after cancel error = nil, want the cancellation")
	} else if agentErr := (*domain.AgentError)(nil); errors.As(err, &agentErr) && agentErr.Kind == domain.ErrTurnTimeout {
		t.Errorf("runBoundedTurn() after a parent cancel = %v, want a cancellation, not a timeout", err)
	}
}
