package pricing

import "github.com/sortie-ai/sortie/internal/domain"

// CostStatus says how far a cost figure can be trusted.
type CostStatus string

const (
	// CostEstimated marks a cost priced from every class the usage spent.
	CostEstimated CostStatus = "estimated"

	// CostPartial marks a cost that leaves out usage the run could not
	// account for.
	CostPartial CostStatus = "partial"

	// CostUnpriced marks usage that spent tokens but has no rate to price it.
	CostUnpriced CostStatus = "unpriced"

	// CostUnmeasured marks usage the agent never reported.
	CostUnmeasured CostStatus = "unmeasured"
)

// PriceSource names the rate source that priced a part.
type PriceSource string

// SourceOperator is the operator's token_rates section.
const SourceOperator PriceSource = "operator"

// Spend is the usage of one finished run or one running session.
type Spend struct {
	// Kind is the dispatch's agent kind, the key into [TokenRates].
	Kind string

	// Measured is false when the agent reported no usage at all.
	Measured bool

	Totals domain.TokenUsage

	// Parts is the usage per model and sums to Totals class by class.
	Parts map[string]domain.TokenUsage

	// UnaccountedTurns counts turns that spent tokens the agent did not
	// report. [PriceSession] ignores it.
	UnaccountedTurns int
}

// PartPrice is the price of one model's share of a run.
type PartPrice struct {
	CostUSD *float64

	// Status is [CostEstimated] or [CostUnpriced].
	Status CostStatus

	// Source is empty when Status is [CostUnpriced].
	Source PriceSource

	// TableVersion is the [TokenRates.Version] of the rates that priced the
	// part, empty when Source is empty.
	TableVersion string
}

// Price is the cost of a finished run and of each of its parts.
type Price struct {
	CostUSD *float64
	Status  CostStatus

	// Parts holds one entry per [Spend.Parts] key, and is nil when the run
	// is unmeasured, spent nothing, or spent tokens in no class.
	Parts map[string]PartPrice
}

// PriceRun prices a finished run under rates. A kind's entry prices the run
// once, on its summed tokens, so the run's cost is not the sum of its parts'
// costs: the fresh-input floor applies per priced usage and would otherwise
// move every multi-model run. PriceRun is pure and never mutates its inputs.
func PriceRun(s Spend, rates TokenRates) Price {
	if !s.Measured {
		return Price{Status: CostUnmeasured}
	}
	if !hasSpent(s.Totals) {
		zero := 0.0
		return Price{CostUSD: &zero, Status: CostEstimated}
	}
	if !hasSpentInClass(s.Totals) {
		return Price{Status: CostUnpriced}
	}

	entry, priceable := completeEntry(rates, s.Kind)
	if !priceable {
		return Price{
			Status: CostUnpriced,
			Parts:  partPrices(s.Parts, func(domain.TokenUsage) PartPrice { return PartPrice{Status: CostUnpriced} }),
		}
	}

	status := CostEstimated
	if s.UnaccountedTurns > 0 {
		status = CostPartial
	}
	version := rates.Version()
	return Price{
		CostUSD: EstimateCost(s.Totals, &entry),
		Status:  status,
		Parts: partPrices(s.Parts, func(part domain.TokenUsage) PartPrice {
			return PartPrice{
				CostUSD:      EstimateCost(part, &entry),
				Status:       CostEstimated,
				Source:       SourceOperator,
				TableVersion: version,
			}
		}),
	}
}

// PriceSession prices a running session's spend so far under rates. A kind
// without a complete entry is unpriced even before the session spends, since
// it still can. PriceSession is pure and never mutates its inputs.
func PriceSession(s Spend, rates TokenRates) (cost *float64, status CostStatus) {
	if !s.Measured {
		return nil, CostUnmeasured
	}
	entry, priceable := completeEntry(rates, s.Kind)
	if !priceable {
		return nil, CostUnpriced
	}
	if !hasSpent(s.Totals) {
		zero := 0.0
		return &zero, CostEstimated
	}
	if !hasSpentInClass(s.Totals) {
		return nil, CostUnpriced
	}
	return EstimateCost(s.Totals, &entry), CostEstimated
}

func completeEntry(rates TokenRates, kind string) (TokenRateConfig, bool) {
	entry, ok := rates[kind]
	return entry, ok && entry.Complete()
}

func partPrices(parts map[string]domain.TokenUsage, price func(domain.TokenUsage) PartPrice) map[string]PartPrice {
	if len(parts) == 0 {
		return nil
	}
	prices := make(map[string]PartPrice, len(parts))
	for model, usage := range parts {
		prices[model] = price(usage)
	}
	return prices
}

func hasSpent(u domain.TokenUsage) bool {
	return hasSpentInClass(u) || u.TotalTokens != 0
}

// hasSpentInClass is false for usage reported as a bare total, which no rate
// can price.
func hasSpentInClass(u domain.TokenUsage) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0
}
