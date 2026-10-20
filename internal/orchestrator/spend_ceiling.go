package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
)

// SpendWindow names a calendar window of the instance spend ceiling.
type SpendWindow string

const (
	SpendWindowDaily   SpendWindow = "daily"
	SpendWindowMonthly SpendWindow = "monthly"
)

// SpendCeilingStatus is the latest evaluation of one configured window.
type SpendCeilingStatus struct {
	Window SpendWindow

	// BudgetUSD is the setting the evaluation read; always above 0.
	BudgetUSD float64

	// UsedUSD is the window's estimated cost, a lower bound unless
	// UsedComplete.
	UsedUSD      float64
	UsedComplete bool

	// Holding is pricing.CeilingReached(UsedUSD, BudgetUSD).
	Holding bool

	// WindowStart and ResetsAt bound the window in UTC; ResetsAt is its
	// exclusive end.
	WindowStart time.Time
	ResetsAt    time.Time

	// HoldingSince is the UTC instant the hold began; zero unless Holding.
	HoldingSince time.Time

	// incompleteWarned is true once the lower-bound Warn ran for this
	// WindowStart.
	incompleteWarned bool
}

// spendStore is the read evaluateSpendCeiling needs.
type spendStore interface {
	UsageCompletedBetween(ctx context.Context, since, until time.Time) (persistence.IssueTokenUsage, error)
}

// spendCeilingParams carries the inputs of one evaluateSpendCeiling call.
type spendCeilingParams struct {
	// Ctx nil reads context.Background().
	Ctx        context.Context
	Store      spendStore
	DailyUSD   float64
	MonthlyUSD float64
	Rates      *pricing.Rates
	Now        time.Time

	// Logger is unscoped so the records carry no issue fields; nil reads
	// slog.Default().
	Logger *slog.Logger
}

// spendWindowBounds returns the UTC calendar window of now: start inclusive,
// end exclusive. end is the reset time.
func spendWindowBounds(window SpendWindow, now time.Time) (start, end time.Time) {
	y, m, d := now.UTC().Date()
	if window == SpendWindowMonthly {
		return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC), time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// windowUsage folds the running sessions into the usage of the window's
// finished runs. A session that has reported nothing yet adds no cost and
// keeps the spend complete; a session whose kind reports no figure at all
// makes it a lower bound.
func windowUsage(rows persistence.IssueTokenUsage, running map[string]*RunningEntry, rates *pricing.Rates) persistence.IssueTokenUsage {
	u := rows
	for _, entry := range running {
		u.UnaccountedTurns += entry.UnaccountedTurns
		if !entry.UsageArrival.ReportsAnyFigure() {
			u.UnmeasuredSessions++
			continue
		}
		if !entry.UsageMeasured {
			continue
		}
		s := entry.sessionCost(rates)
		switch s.Status {
		case pricing.CostEstimated, pricing.CostPartial:
			if s.USD != nil {
				u.RecordedCostUSD += *s.USD
			}
			if s.Spent {
				u.PricedSessions++
			}
			if s.Status == pricing.CostPartial {
				u.PartialSessions++
			}
		case pricing.CostUnpriced:
			if s.Spent {
				u.UnpricedSessions++
			}
		}
	}
	return u
}

func spendCeilingSetting(window SpendWindow) string {
	if window == SpendWindowMonthly {
		return "agent.max_monthly_cost_usd"
	}
	return "agent.max_daily_cost_usd"
}

func spendSkipReason(window SpendWindow) SkipReason {
	if window == SpendWindowMonthly {
		return SkipMonthlySpendCeiling
	}
	return SkipDailySpendCeiling
}

func findSpendStatus(statuses []SpendCeilingStatus, window SpendWindow) (SpendCeilingStatus, bool) {
	for _, s := range statuses {
		if s.Window == window {
			return s, true
		}
	}
	return SpendCeilingStatus{}, false
}

// holdsSpendWindow reports whether statuses holds window at start.
func holdsSpendWindow(statuses []SpendCeilingStatus, window SpendWindow, start time.Time) bool {
	s, ok := findSpendStatus(statuses, window)
	return ok && s.Holding && s.WindowStart.Equal(start)
}

// evaluateSpendCeiling refreshes state.SpendCeilings from one read per
// configured window and returns the skip reason of the window that holds
// dispatch, the monthly one when both do. It reads nothing while neither
// setting is above 0. Must run on the single-writer event loop.
func evaluateSpendCeiling(state *State, p spendCeilingParams) SkipReason {
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}

	prev := state.SpendCeilings
	var next []SpendCeilingStatus
	var usages []persistence.IssueTokenUsage
	windows := []struct {
		window  SpendWindow
		ceiling float64
	}{{SpendWindowDaily, p.DailyUSD}, {SpendWindowMonthly, p.MonthlyUSD}}

	for _, w := range windows {
		if w.ceiling <= 0 {
			continue
		}
		start, end := spendWindowBounds(w.window, p.Now)
		old, had := findSpendStatus(prev, w.window)
		same := had && old.WindowStart.Equal(start)

		rows, err := p.Store.UsageCompletedBetween(ctx, start, end)
		if err != nil {
			log.Warn("spend ceiling check failed, keeping the last result",
				slog.String("window", string(w.window)),
				slog.Any("error", err),
			)
			status := SpendCeilingStatus{Window: w.window, BudgetUSD: w.ceiling, WindowStart: start, ResetsAt: end}
			if same {
				status = old
			}
			next = append(next, status)
			usages = append(usages, persistence.IssueTokenUsage{})
			continue
		}

		u := windowUsage(rows, state.Running, p.Rates)
		used, costStatus := pricing.IssueCost(recordedOf(u), pricing.SessionCost{}, !u.SpendIncomplete())
		status := SpendCeilingStatus{
			Window:       w.window,
			BudgetUSD:    w.ceiling,
			UsedUSD:      used,
			UsedComplete: costStatus == pricing.CostEstimated,
			Holding:      pricing.CeilingReached(used, w.ceiling),
			WindowStart:  start,
			ResetsAt:     end,
		}
		switch {
		case status.Holding && same && old.Holding:
			status.HoldingSince = old.HoldingSince
		case status.Holding:
			status.HoldingSince = p.Now.UTC()
		case !status.UsedComplete:
			status.incompleteWarned = same && old.incompleteWarned
		}
		next = append(next, status)
		usages = append(usages, u)
	}
	state.SpendCeilings = next

	reason := SkipNone
	for _, s := range next {
		if s.Holding {
			reason = spendSkipReason(s.Window)
		}
	}

	for _, e := range prev {
		if !e.Holding || holdsSpendWindow(next, e.Window, e.WindowStart) {
			continue
		}
		attrs := []any{slog.String("window", string(e.Window)), slog.Float64("budget_usd", 0)}
		if cur, ok := findSpendStatus(next, e.Window); ok {
			attrs = []any{
				slog.String("window", string(e.Window)),
				slog.Float64("budget_usd", cur.BudgetUSD),
				slog.Float64("used_usd", cur.UsedUSD),
			}
		}
		attrs = append(attrs,
			slog.String("ceiling_setting", spendCeilingSetting(e.Window)),
			slog.Bool("dispatch_held", reason != SkipNone),
		)
		log.Info("spend ceiling hold ended", attrs...)
	}

	for i := range next {
		s := &next[i]
		switch {
		case s.Holding:
			if !holdsSpendWindow(prev, s.Window, s.WindowStart) {
				log.Warn("spend ceiling reached, holding dispatch",
					slog.String("window", string(s.Window)),
					slog.Float64("used_usd", s.UsedUSD),
					slog.Bool("used_usd_complete", s.UsedComplete),
					slog.Float64("budget_usd", s.BudgetUSD),
					slog.String("ceiling_setting", spendCeilingSetting(s.Window)),
					slog.Time("resets_at", s.ResetsAt.UTC()),
				)
			}
		case !s.UsedComplete && !s.incompleteWarned:
			s.incompleteWarned = true
			u := usages[i]
			log.Warn("spend ceiling cannot be fully evaluated, allowing dispatch",
				slog.String("window", string(s.Window)),
				slog.Float64("used_usd", s.UsedUSD),
				slog.Float64("budget_usd", s.BudgetUSD),
				slog.Int("unmeasured_sessions", u.UnmeasuredSessions),
				slog.Int("unaccounted_turns", u.UnaccountedTurns),
				slog.Int("partial_sessions", u.PartialSessions),
				slog.Int("unpriced_sessions", u.UnpricedSessions),
			)
		}
	}
	return reason
}
