package agentcore

import "github.com/sortie-ai/sortie/internal/domain"

// RunUsage tracks normalized token usage across every turn of one agent
// session. It combines a settled total (the latest run-cumulative figure
// the adapter set) with a provisional contribution from the turn
// currently in flight, and never returns a snapshot lower than any
// snapshot it has already returned, so a caller that reports each
// method's result observes a monotonically non-decreasing sequence per
// component even when a later provisional estimate would otherwise be
// smaller than an earlier one.
//
// Not safe for concurrent use; each adapter session constructs its own
// RunUsage in StartSession and uses it on the single goroutine that
// executes RunTurn for that session.
type RunUsage struct {
	settled     domain.TokenUsage
	provisional domain.TokenUsage
	high        domain.TokenUsage
}

// NewRunUsage returns a zero-value RunUsage ready for use.
func NewRunUsage() *RunUsage {
	return &RunUsage{}
}

// SetTurnProvisional replaces the in-flight turn's contribution with
// turn and returns the resulting run-cumulative snapshot (settled plus
// the new provisional contribution), raised against the highest
// snapshot returned so far.
func (u *RunUsage) SetTurnProvisional(turn domain.TokenUsage) domain.TokenUsage {
	u.provisional = clamp(turn)
	return u.raise(add(u.settled, u.provisional))
}

// SetRunCumulative replaces the settled total with run outright, clears
// the in-flight turn's provisional contribution, and returns the new
// settled total, raised against the highest snapshot returned so far.
func (u *RunUsage) SetRunCumulative(run domain.TokenUsage) domain.TokenUsage {
	u.settled = clamp(run)
	u.provisional = domain.TokenUsage{}
	return u.raise(u.settled)
}

// Snapshot returns the highest run-cumulative snapshot returned so far
// by any method, without modifying u. Returns the zero value before any
// other method has been called.
func (u *RunUsage) Snapshot() domain.TokenUsage {
	return u.high
}

// raise sets u.high to the componentwise maximum of u.high and snapshot
// and returns the result.
func (u *RunUsage) raise(snapshot domain.TokenUsage) domain.TokenUsage {
	u.high = MaxUsage(u.high, snapshot)
	return u.high
}

// ResumeBaseline separates what a run added to a resumable session from
// the usage the runtime's session counter already held when the run
// started. The counter is cumulative over every launch on the session,
// so a reading alone overstates the run.
//
// The zero value is an unknown baseline; the first reading that is not all
// zero fixes it by estimate. One value serves one session on the goroutine
// that executes its turns and is never reset between turns, because the
// baseline is what makes the readings of later turns comparable.
type ResumeBaseline struct {
	baseline    domain.TokenUsage
	latest      domain.TokenUsage
	beforeReset domain.TokenUsage
	known       bool
}

// NewResumeBaseline returns a baseline starting from resume, floored at
// zero in every component. A nil resume returns an unknown baseline.
func NewResumeBaseline(resume *domain.TokenUsage) *ResumeBaseline {
	if resume == nil {
		return &ResumeBaseline{}
	}
	start := clamp(*resume)
	return &ResumeBaseline{baseline: start, latest: start, known: true}
}

// Observe records one reading of the session counter and returns the
// run's usage so far. own is the part of the reading the runtime
// attributes to the current request or turn, or nil when it attributes
// none.
//
// A reading with every counted component zero has no effect. When the
// baseline is unknown, or the reading is below the latest one in any
// counted component, the counter is taken to have started over: the
// usage so far is kept, the baseline becomes the reading less own, and
// estimated is true because own may not cover everything the reading
// added. Otherwise estimated is false.
func (b *ResumeBaseline) Observe(reading domain.TokenUsage, own *domain.TokenUsage) (run domain.TokenUsage, estimated bool) {
	if isZeroReading(reading) {
		return b.Run(), false
	}
	if !b.known || fellBelow(reading, b.latest) {
		b.beforeReset = b.Run()
		b.baseline = reading
		if own != nil {
			b.baseline = SubtractUsage(reading, *own)
		}
		b.known = true
		estimated = true
	}
	b.latest = reading
	return b.Run(), estimated
}

// Run returns the usage the run added to the session: the usage kept
// across counter resets plus the latest reading less the baseline. It
// is zero while the baseline is unknown.
func (b *ResumeBaseline) Run() domain.TokenUsage {
	if !b.known {
		return domain.TokenUsage{}
	}
	return AddUsage(b.beforeReset, SubtractUsage(b.latest, b.baseline))
}

// Resume returns a copy of the latest reading, which a later run
// resuming the session starts from, or nil while the baseline is
// unknown.
func (b *ResumeBaseline) Resume() *domain.TokenUsage {
	if !b.known {
		return nil
	}
	latest := b.latest
	return &latest
}

func isZeroReading(usage domain.TokenUsage) bool {
	return usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0
}

func fellBelow(reading, latest domain.TokenUsage) bool {
	return reading.InputTokens < latest.InputTokens ||
		reading.OutputTokens < latest.OutputTokens ||
		reading.CacheReadTokens < latest.CacheReadTokens ||
		reading.CacheWriteTokens < latest.CacheWriteTokens
}

// clamp floors every negative component of usage at zero and recomputes
// TotalTokens as InputTokens plus OutputTokens.
func clamp(usage domain.TokenUsage) domain.TokenUsage {
	return SubtractUsage(usage, domain.TokenUsage{})
}

// add returns the componentwise sum of a and b, with TotalTokens
// recomputed as InputTokens plus OutputTokens.
func add(a, b domain.TokenUsage) domain.TokenUsage {
	return AddUsage(a, b)
}

// SubtractUsage returns the componentwise difference a minus b, floored
// at zero in every component, with TotalTokens recomputed as
// InputTokens plus OutputTokens.
func SubtractUsage(a, b domain.TokenUsage) domain.TokenUsage {
	diff := domain.TokenUsage{
		InputTokens:      max(a.InputTokens-b.InputTokens, 0),
		OutputTokens:     max(a.OutputTokens-b.OutputTokens, 0),
		CacheReadTokens:  max(a.CacheReadTokens-b.CacheReadTokens, 0),
		CacheWriteTokens: max(a.CacheWriteTokens-b.CacheWriteTokens, 0),
	}
	diff.TotalTokens = diff.InputTokens + diff.OutputTokens
	return diff
}

// MaxUsage returns the componentwise maximum of a and b, with
// TotalTokens recomputed as InputTokens plus OutputTokens.
func MaxUsage(a, b domain.TokenUsage) domain.TokenUsage {
	result := domain.TokenUsage{
		InputTokens:      max(a.InputTokens, b.InputTokens),
		OutputTokens:     max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:  max(a.CacheReadTokens, b.CacheReadTokens),
		CacheWriteTokens: max(a.CacheWriteTokens, b.CacheWriteTokens),
	}
	result.TotalTokens = result.InputTokens + result.OutputTokens
	return result
}

// AddUsage returns the componentwise sum of a and b, with TotalTokens
// recomputed as InputTokens plus OutputTokens.
func AddUsage(a, b domain.TokenUsage) domain.TokenUsage {
	sum := domain.TokenUsage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
	return sum
}
