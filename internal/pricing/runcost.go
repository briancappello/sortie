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

const (
	// SourceOperator is the operator's token_rates section, alone or laid
	// over a table.
	SourceOperator PriceSource = "operator"

	// SourceFetched is the shared price table downloaded at runtime.
	SourceFetched PriceSource = "fetched"

	// SourceEmbedded is the price table compiled into the binary.
	SourceEmbedded PriceSource = "embedded"
)

// Spend is the usage of one finished run or one running session.
type Spend struct {
	// Kind is the dispatch's agent kind, the key of a kind entry in [Rates].
	Kind string

	// Model is the model tokens arriving now are attributed to: the model
	// the session's events named last, else its configured model. Only
	// [PriceSession] reads it, for a session that has spent nothing.
	Model string

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

	// Status is [CostEstimated], [CostPartial], or [CostUnpriced].
	Status CostStatus

	// Source is empty when Status is [CostUnpriced].
	Source PriceSource

	// TableVersion is the version of the rates that priced the part, empty
	// when Source is empty.
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

// PriceRun prices a finished run under r. A kind entry for [Spend.Kind]
// prices the run once, on its summed tokens, so the run's cost is not the
// sum of its parts' costs: the fresh-input floor applies per priced usage
// and would otherwise move every multi-model run. Without one, each part is
// priced on its own tokens and the run's cost is their sum. PriceRun is
// pure and never mutates its inputs.
func PriceRun(s Spend, r *Rates) Price {
	if !s.Measured {
		return Price{Status: CostUnmeasured}
	}
	status := CostEstimated
	if s.UnaccountedTurns > 0 {
		status = CostPartial
	}
	if !hasSpent(s.Totals) {
		zero := 0.0
		return Price{CostUSD: &zero, Status: status}
	}
	if !hasSpentInClass(s.Totals) {
		return Price{Status: CostUnpriced}
	}

	if r != nil && r.kinds[s.Kind] {
		return priceRunByKind(s, r, status)
	}
	if len(s.Parts) == 0 {
		return Price{Status: CostUnpriced}
	}

	parts, cost, priced, degraded := priceParts(r, s.Parts)
	if !priced {
		return Price{Status: CostUnpriced, Parts: parts}
	}
	if degraded {
		status = CostPartial
	}
	return Price{CostUSD: &cost, Status: status, Parts: parts}
}

func priceRunByKind(s Spend, r *Rates, status CostStatus) Price {
	entry, priceable := completeEntry(r, s.Kind)
	if !priceable {
		return Price{
			Status: CostUnpriced,
			Parts:  partPrices(s.Parts, func(domain.TokenUsage) PartPrice { return PartPrice{Status: CostUnpriced} }),
		}
	}

	return Price{
		CostUSD: EstimateCost(s.Totals, &entry),
		Status:  status,
		Parts: partPrices(s.Parts, func(part domain.TokenUsage) PartPrice {
			return PartPrice{
				CostUSD:      EstimateCost(part, &entry),
				Status:       CostEstimated,
				Source:       SourceOperator,
				TableVersion: r.version,
			}
		}),
	}
}

// PriceSession prices a running session's spend so far under r. A session
// under an incomplete kind entry is unpriced even before it spends, since it
// still can. PriceSession is pure and never mutates its inputs.
func PriceSession(s Spend, r *Rates) (cost *float64, status CostStatus) {
	if !s.Measured {
		return nil, CostUnmeasured
	}
	if r != nil && r.kinds[s.Kind] {
		return priceSessionByKind(s, r)
	}
	if !hasSpent(s.Totals) {
		if !r.resolve(s.Model).pricesAnything() {
			return nil, CostUnpriced
		}
		zero := 0.0
		return &zero, CostEstimated
	}
	if !hasSpentInClass(s.Totals) || len(s.Parts) == 0 {
		return nil, CostUnpriced
	}

	_, total, priced, degraded := priceParts(r, s.Parts)
	switch {
	case !priced:
		return nil, CostUnpriced
	case degraded:
		return &total, CostPartial
	}
	return &total, CostEstimated
}

func priceSessionByKind(s Spend, r *Rates) (cost *float64, status CostStatus) {
	entry, priceable := completeEntry(r, s.Kind)
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

// priceParts prices every part at the rates of its own model and sums their
// costs. priced is false when no part that spent in a class got a price, and
// degraded is true when such a part is partial or unpriced.
func priceParts(r *Rates, parts map[string]domain.TokenUsage) (prices map[string]PartPrice, cost float64, priced, degraded bool) {
	prices = make(map[string]PartPrice, len(parts))
	for model, usage := range parts {
		part := pricePart(usage, r.resolve(model))
		prices[model] = part
		if part.CostUSD != nil {
			cost += *part.CostUSD
		}
		if !hasSpentInClass(usage) {
			continue
		}
		switch part.Status {
		case CostEstimated:
			priced = true
		case CostPartial:
			priced, degraded = true, true
		default:
			degraded = true
		}
	}
	return prices, cost, priced, degraded
}

// pricePart prices one model's usage at res; a nil res holds no rate.
// A class with tokens and no rate is missing and is never priced at another
// class's rate.
func pricePart(usage domain.TokenUsage, res *resolution) PartPrice {
	if !hasSpentInClass(usage) {
		if usage.TotalTokens != 0 {
			return PartPrice{Status: CostUnpriced}
		}
		zero := 0.0
		part := PartPrice{CostUSD: &zero, Status: CostEstimated}
		if res != nil {
			part.Source, part.TableVersion = res.source, res.version
		}
		return part
	}
	if res == nil {
		return PartPrice{Status: CostUnpriced}
	}

	tokens := [rateClasses]int64{
		rateInput:      max(usage.InputTokens-usage.CacheReadTokens-usage.CacheWriteTokens, 0),
		rateOutput:     usage.OutputTokens,
		rateCacheRead:  usage.CacheReadTokens,
		rateCacheWrite: usage.CacheWriteTokens,
	}
	var cost float64
	priced, missing := false, false
	for _, class := range [...]int{rateInput, rateCacheRead, rateCacheWrite, rateOutput} {
		if tokens[class] <= 0 {
			continue
		}
		if res.perToken[class] == nil {
			missing = true
			continue
		}
		cost += float64(tokens[class]) * *res.perToken[class]
		priced = true
	}
	if !priced {
		return PartPrice{Status: CostUnpriced}
	}
	status := CostEstimated
	if missing {
		status = CostPartial
	}
	return PartPrice{CostUSD: &cost, Status: status, Source: res.source, TableVersion: res.version}
}

func completeEntry(r *Rates, kind string) (TokenRateConfig, bool) {
	entry := r.entries[kind]
	return entry, entry.Complete()
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
