package server

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/pricing"
)

// runningEntryCost prices usage under kind's configured rate. priced is
// true only when rates holds an entry for kind and EstimateCost returns
// a non-nil figure for it.
func runningEntryCost(kind string, usage domain.TokenUsage, rates pricing.TokenRates) (cost *float64, priced bool) {
	rc, ok := rates[kind]
	if !ok {
		return nil, false
	}
	c := pricing.EstimateCost(usage, &rc)
	return c, c != nil
}

// Cost states served on running rows. An estimate is exact only in the
// estimated state; partial is a lower bound.
const (
	costStatusEstimated  = "estimated"
	costStatusUnpriced   = "unpriced"
	costStatusUnmeasured = "unmeasured"
	costStatusPartial    = "partial"
)

// sessionCostOf prices the running session of e under rates and names the
// state of that estimate. It prices through [runningEntryCost] with the
// figures [activeCostTotal] uses, so a row agrees with the aggregate keys.
func sessionCostOf(e orchestrator.SnapshotRunningEntry, rates pricing.TokenRates) (cost *float64, status string) {
	if !e.UsageMeasured {
		return nil, costStatusUnmeasured
	}
	usage := domain.TokenUsage{
		InputTokens:      e.AgentInputTokens,
		OutputTokens:     e.AgentOutputTokens,
		CacheReadTokens:  e.CacheReadTokens,
		CacheWriteTokens: e.CacheWriteTokens,
	}
	if c, priced := runningEntryCost(e.AgentKind, usage, rates); priced {
		return c, costStatusEstimated
	}
	return nil, costStatusUnpriced
}

// issueCostOf sums the estimates of the priced parts of e's issue: each
// completed kind and the running session, whose estimate arrives as
// sessionCost and sessionStatus. See [issueCostFromKinds] for the closing
// decision.
func issueCostOf(e orchestrator.SnapshotRunningEntry, usedTokens *int64, usedComplete bool, sessionCost *float64, sessionStatus string, rates pricing.TokenRates) (cost *float64, status string) {
	var byKind map[string]domain.TokenUsage
	if e.IssueUsage != nil {
		byKind = e.IssueUsage.ByKind
	}
	session := sessionCostPart{cost: sessionCost, status: sessionStatus, spent: e.AgentTotalTokens > 0}
	return issueCostFromKinds(byKind, usedTokens, usedComplete, session, rates)
}

// sessionCostPart is the running session's share of an issue's cost. The
// zero value is no session.
type sessionCostPart struct {
	cost   *float64
	status string
	spent  bool
}

// issueCostFromKinds sums the estimates of the priced parts of an issue:
// each completed kind in byKind and the running session. The result is
// estimated only when usedComplete says the priced parts cover the whole
// spend, and partial when it is a lower bound. A part that spent nothing
// prices nothing, so a running session at zero tokens neither makes an
// unpriced issue partial nor a fully priced one unpriced.
func issueCostFromKinds(byKind map[string]domain.TokenUsage, usedTokens *int64, usedComplete bool, session sessionCostPart, rates pricing.TokenRates) (cost *float64, status string) {
	if usedTokens == nil {
		return nil, costStatusUnmeasured
	}

	var total float64
	var pricedAny, unpricedAny bool
	for _, kind := range slices.Sorted(maps.Keys(byKind)) {
		if c, priced := runningEntryCost(kind, byKind[kind], rates); priced {
			total += *c
			pricedAny = true
		} else {
			unpricedAny = true
		}
	}

	switch session.status {
	case costStatusEstimated:
		total += *session.cost
		pricedAny = pricedAny || session.spent
	case costStatusUnpriced:
		unpricedAny = unpricedAny || session.spent
	}

	switch {
	case usedComplete && !unpricedAny:
		return &total, costStatusEstimated
	case pricedAny:
		return &total, costStatusPartial
	case unpricedAny:
		return nil, costStatusUnpriced
	default:
		return nil, costStatusUnmeasured
	}
}

// runCostOf prices a finished run under rates and names the state of that
// estimate. A run that spent nothing is a free estimate without a rate,
// because its figure is final.
func runCostOf(e RunHistoryEntry, rates pricing.TokenRates) (cost *float64, status string) {
	if !e.TokensMeasured {
		return nil, costStatusUnmeasured
	}
	if e.Tokens == (domain.TokenUsage{}) {
		return new(0.0), costStatusEstimated
	}
	c, priced := runningEntryCost(e.AgentKind, e.Tokens, rates)
	if !priced {
		return nil, costStatusUnpriced
	}
	if e.UnaccountedTurns > 0 {
		return c, costStatusPartial
	}
	return c, costStatusEstimated
}

// activeCostTotal sums the estimated USD cost of running, measured
// sessions with a configured rate, and counts the measured sessions
// left out for want of one. anySet is false when no session priced,
// distinguishing that from a priced total of zero.
func activeCostTotal(running []orchestrator.SnapshotRunningEntry, tokenRates pricing.TokenRates) (total float64, anySet bool, unpriced int) {
	for _, e := range running {
		if !e.UsageMeasured {
			continue
		}
		usage := domain.TokenUsage{
			InputTokens:      e.AgentInputTokens,
			OutputTokens:     e.AgentOutputTokens,
			CacheReadTokens:  e.CacheReadTokens,
			CacheWriteTokens: e.CacheWriteTokens,
		}
		if c, priced := runningEntryCost(e.AgentKind, usage, tokenRates); priced {
			total += *c
			anySet = true
		} else {
			unpriced++
		}
	}
	return total, anySet, unpriced
}

// FormatCost formats a USD cost value as a string with two decimal places.
// Values >= 1000 receive comma thousand separators (e.g. "$1,234.56").
// Rounding is performed on the integer-cents representation to avoid
// float splitting artifacts near boundaries (e.g. 999.999 -> "$1,000.00").
func FormatCost(v float64) string {
	cents := int64(math.Round(v * 100))
	dollars := cents / 100
	remainder := cents % 100
	if dollars >= 1000 {
		return fmt.Sprintf("$%s.%02d", FormatInt(dollars), remainder)
	}
	return fmt.Sprintf("$%d.%02d", dollars, remainder)
}
