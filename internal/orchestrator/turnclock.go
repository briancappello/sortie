package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// errTurnDeadline is the cancellation cause of a turn whose turn clock
// expired.
var errTurnDeadline = fmt.Errorf("turn deadline: %w", context.DeadlineExceeded)

// turnClock bounds one run's turns. A turn's deadline is its start plus
// its bound plus the time the run spent held during that turn, and a
// held run has no deadline at all. The event loop holds and releases
// the clock; the worker goroutine runs turns against it. One clock lives
// as long as its running entry.
type turnClock struct {
	now func() time.Time

	mu         sync.Mutex
	held       bool
	heldSince  time.Time
	active     bool
	turnStart  time.Time
	bound      time.Duration
	heldInTurn time.Duration
	changed    chan struct{}
}

func newTurnClock() *turnClock {
	return &turnClock{now: time.Now, changed: make(chan struct{})}
}

// hold starts a hold at now. A hold already in effect keeps its start.
func (c *turnClock) hold(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held {
		return
	}
	c.held = true
	c.heldSince = now
	c.signalLocked()
}

// release ends a hold at now, crediting the part of it that fell inside
// the current turn to that turn's deadline.
func (c *turnClock) release(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.held {
		return
	}
	if c.active {
		c.heldInTurn += heldOverlap(c.heldSince, c.turnStart, now)
	}
	c.held = false
	c.signalLocked()
}

// heldOverlap returns the part of a hold from heldSince to now that lies
// after turnStart.
func heldOverlap(heldSince, turnStart, now time.Time) time.Duration {
	from := heldSince
	if turnStart.After(from) {
		from = turnStart
	}
	if now.Before(from) {
		return 0
	}
	return now.Sub(from)
}

func (c *turnClock) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// deadline reports the current turn's deadline, and held true when the
// run is held, in which case the turn has none.
func (c *turnClock) deadline() (deadline time.Time, held bool, changed <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnStart.Add(c.bound + c.heldInTurn), c.held, c.changed
}

// watch starts a turn of the given bound and calls expire once, from
// its own goroutine, when the turn's deadline passes while the run is
// not held. The returned stop ends the turn; expire is never called
// after stop returns.
func (c *turnClock) watch(ctx context.Context, bound time.Duration, expire func()) (stop func()) {
	c.mu.Lock()
	c.active = true
	c.turnStart = c.now()
	c.bound = bound
	c.heldInTurn = 0
	c.mu.Unlock()

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			deadline, held, changed := c.deadline()
			var timer *time.Timer
			var fire <-chan time.Time
			if !held {
				wait := deadline.Sub(c.now())
				if wait <= 0 {
					expire()
					return
				}
				timer = time.NewTimer(wait)
				fire = timer.C
			}
			select {
			case <-done:
			case <-ctx.Done():
			case <-changed:
			case <-fire:
			}
			if timer != nil {
				timer.Stop()
			}
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	return func() {
		close(done)
		<-finished
		c.mu.Lock()
		c.active = false
		c.mu.Unlock()
	}
}

// turnContext is a turn's context: canceled like its parent, and also
// when the turn clock expires, in which case Err reports
// [context.DeadlineExceeded] as a deadline context would. Deadline
// reports the clock's current deadline, which a hold extends, and none
// while the run is held.
type turnContext struct {
	context.Context
	clock *turnClock
}

// newTurnContext returns the context a turn of the given bound runs
// under and the function that ends the turn.
func newTurnContext(parent context.Context, clock *turnClock, bound time.Duration) (context.Context, func()) {
	inner, cancel := context.WithCancelCause(parent)
	stop := clock.watch(inner, bound, func() { cancel(errTurnDeadline) })
	return turnContext{Context: inner, clock: clock}, func() {
		stop()
		cancel(nil)
	}
}

func (c turnContext) Deadline() (time.Time, bool) {
	deadline, held, _ := c.clock.deadline()
	parent, ok := c.Context.Deadline()
	switch {
	case held:
		return parent, ok
	case ok && parent.Before(deadline):
		return parent, true
	default:
		return deadline, true
	}
}

func (c turnContext) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), errTurnDeadline) {
		return context.DeadlineExceeded
	}
	return err
}
