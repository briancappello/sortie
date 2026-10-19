package server

import (
	"fmt"
	"math"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// Cost states served on running rows. An estimate is exact only in the
// estimated state; partial is a lower bound.
const (
	costStatusEstimated  = "estimated"
	costStatusUnpriced   = "unpriced"
	costStatusUnmeasured = "unmeasured"
	costStatusPartial    = "partial"
)

// servedCost returns a recorded or live figure as it is served. An empty
// status is a run recorded without a price, which reads as unpriced rather
// than as a guessed cost.
func servedCost(cost *float64, status string) (*float64, string) {
	if status == "" {
		return nil, costStatusUnpriced
	}
	return cost, status
}

func liveCostOf(e orchestrator.SnapshotRunningEntry) (cost *float64, status string) {
	return servedCost(e.EstimatedCostUSD, e.CostStatus)
}

// sessionCostPart is the running session's share of an issue's cost. The
// zero value is no session.
type sessionCostPart struct {
	cost   *float64
	status string
	spent  bool
}

func sessionPartOf(e orchestrator.SnapshotRunningEntry) sessionCostPart {
	cost, status := liveCostOf(e)
	spent := e.AgentInputTokens != 0 || e.AgentOutputTokens != 0 || e.AgentTotalTokens != 0 ||
		e.CacheReadTokens != 0 || e.CacheWriteTokens != 0
	return sessionCostPart{cost: cost, status: status, spent: spent}
}

// issueCost folds the recorded cost of an issue's completed sessions and its
// running session into one cost and state. The result is estimated only when
// usedComplete says the priced parts cover the whole spend, and partial when
// it is a lower bound. A part that spent nothing prices nothing, so a running
// session at zero tokens neither makes an unpriced issue partial nor a fully
// priced one unpriced.
func issueCost(completed *orchestrator.SnapshotIssueUsage, usedTokens *int64, usedComplete bool, session sessionCostPart) (cost *float64, status string) {
	if usedTokens == nil {
		return nil, costStatusUnmeasured
	}

	var recorded orchestrator.SnapshotIssueUsage
	if completed != nil {
		recorded = *completed
	}
	total := recorded.RecordedCostUSD
	priced := recorded.PricedSessions > 0
	partial := recorded.PartialSessions > 0
	unpriced := recorded.UnpricedSessions > 0

	switch session.status {
	case costStatusEstimated, costStatusPartial:
		if session.cost != nil {
			total += *session.cost
		}
		priced = priced || session.spent
		partial = partial || session.status == costStatusPartial
	case costStatusUnpriced:
		unpriced = unpriced || session.spent
	}

	switch {
	case usedComplete && !unpriced && !partial:
		return &total, costStatusEstimated
	case priced:
		return &total, costStatusPartial
	case unpriced:
		return nil, costStatusUnpriced
	default:
		return nil, costStatusUnmeasured
	}
}

// activeCostTotal sums the live estimates of the running sessions priced
// estimated or partial, and counts the sessions priced unpriced. anySet is
// false when no session priced, distinguishing that from a priced total of
// zero.
func activeCostTotal(running []orchestrator.SnapshotRunningEntry) (total float64, anySet bool, unpriced int) {
	for _, e := range running {
		cost, status := liveCostOf(e)
		switch status {
		case costStatusEstimated, costStatusPartial:
			if cost != nil {
				total += *cost
				anySet = true
			}
		case costStatusUnpriced:
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
