package pricing

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func fixtureRates(inputPerMtok float64) TokenRates {
	return TokenRates{"k": {
		InputPerMtok:      new(inputPerMtok),
		OutputPerMtok:     new(10.0),
		CacheReadPerMtok:  new(0.5),
		CacheWritePerMtok: new(4.0),
	}}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	return string(encoded)
}

var (
	usageS     = domain.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 2_000_000}
	usagePartA = domain.TokenUsage{InputTokens: 1_000_000, CacheReadTokens: 2_000_000}
	usagePartB = domain.TokenUsage{InputTokens: 3_000_000, OutputTokens: 1_000_000}
	usageM     = domain.TokenUsage{InputTokens: 4_000_000, OutputTokens: 1_000_000, CacheReadTokens: 2_000_000}
	onlyTotal  = domain.TokenUsage{TotalTokens: 500}
)

func operatorOf(cost float64) PartPrice {
	return PartPrice{CostUSD: new(cost), Status: CostEstimated, Source: SourceOperator, TableVersion: fixtureRates(2).Version()}
}

func TestPriceRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spend Spend
		want  Price
	}{
		{
			name:  "unmeasured run",
			spend: Spend{Kind: "k", Totals: usageS},
			want:  Price{Status: CostUnmeasured},
		},
		{
			name:  "measured run that spent nothing",
			spend: Spend{Kind: "u", Measured: true},
			want:  Price{CostUSD: new(0.0), Status: CostEstimated},
		},
		{
			name:  "run that spent nothing with an unaccounted turn",
			spend: Spend{Kind: "u", Measured: true, UnaccountedTurns: 1},
			want:  Price{CostUSD: new(0.0), Status: CostPartial},
		},
		{
			name:  "run that spent only total_tokens",
			spend: Spend{Kind: "k", Measured: true, Totals: onlyTotal},
			want:  Price{Status: CostUnpriced},
		},
		{
			name:  "kind without an entry",
			spend: Spend{Kind: "u", Measured: true, Totals: usageS, Parts: map[string]domain.TokenUsage{"m": usageS}},
			want:  Price{Status: CostUnpriced, Parts: map[string]PartPrice{"m": {Status: CostUnpriced}}},
		},
		{
			name:  "S",
			spend: Spend{Kind: "k", Measured: true, Totals: usageS, Parts: map[string]domain.TokenUsage{"m": usageS}},
			want:  Price{CostUSD: new(12.0), Status: CostEstimated, Parts: map[string]PartPrice{"m": operatorOf(12)}},
		},
		{
			name:  "S with one unaccounted turn",
			spend: Spend{Kind: "k", Measured: true, Totals: usageS, UnaccountedTurns: 1},
			want:  Price{CostUSD: new(12.0), Status: CostPartial},
		},
		{
			name: "M prices on summed tokens, not the sum of its parts",
			spend: Spend{Kind: "k", Measured: true, Totals: usageM,
				Parts: map[string]domain.TokenUsage{"a": usagePartA, "b": usagePartB}},
			want: Price{CostUSD: new(15.0), Status: CostEstimated,
				Parts: map[string]PartPrice{"a": operatorOf(1), "b": operatorOf(16)}},
		},
	}

	rates := fixtureRates(2)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := PriceRun(tt.spend, rates)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PriceRun(%s) = %s, want %s", asJSON(t, tt.spend), asJSON(t, got), asJSON(t, tt.want))
			}
		})
	}
}

func TestPriceSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		spend      Spend
		wantCost   *float64
		wantStatus CostStatus
	}{
		{"unmeasured", Spend{Kind: "k", Totals: usageS}, nil, CostUnmeasured},
		{"kind without an entry that spent nothing", Spend{Kind: "u", Measured: true}, nil, CostUnpriced},
		{"kind with an entry that spent nothing", Spend{Kind: "k", Measured: true}, new(0.0), CostEstimated},
		{"only total_tokens", Spend{Kind: "k", Measured: true, Totals: onlyTotal}, nil, CostUnpriced},
		{"S", Spend{Kind: "k", Measured: true, Totals: usageS}, new(12.0), CostEstimated},
	}

	rates := fixtureRates(2)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotCost, gotStatus := PriceSession(tt.spend, rates)

			if !reflect.DeepEqual(gotCost, tt.wantCost) || gotStatus != tt.wantStatus {
				t.Errorf("PriceSession(%s) = %s, %q, want %s, %q",
					asJSON(t, tt.spend), asJSON(t, gotCost), gotStatus, asJSON(t, tt.wantCost), tt.wantStatus)
			}
		})
	}
}
