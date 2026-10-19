package main

import (
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
)

func TestRecordedCostOf(t *testing.T) {
	t.Parallel()

	rates := pricing.TokenRates{"k": {
		InputPerMtok: new(2.0), OutputPerMtok: new(10.0), CacheReadPerMtok: new(0.5), CacheWritePerMtok: new(4.0),
	}}
	estimated := func(usd float64) persistence.ModelCost {
		return persistence.ModelCost{EstimatedCostUSD: &usd, CostStatus: "estimated", PriceSource: "operator", PriceTableVersion: rates.Version()}
	}
	spent := persistence.ModelTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	spentRun := func(kind, reported string) persistence.UnrecordedRun {
		return persistence.UnrecordedRun{
			AgentAdapter: kind, TokensMeasured: true, InputTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 2_000_000,
			ConfiguredModel: "m0", ReportedModel: reported,
		}
	}
	measured := persistence.UnrecordedRun{AgentAdapter: "k", TokensMeasured: true}
	totalOnly := measured
	totalOnly.TotalTokens = 5
	withParts := persistence.UnrecordedRun{
		AgentAdapter: "k", TokensMeasured: true,
		InputTokens: 4_000_000, OutputTokens: 1_000_000, TotalTokens: 5_000_000, CacheReadTokens: 2_000_000,
		Parts: map[string]persistence.ModelTokens{
			"a": {InputTokens: 1_000_000, CacheReadTokens: 2_000_000},
			"b": {InputTokens: 3_000_000, OutputTokens: 1_000_000},
		},
	}

	tests := []struct {
		name string
		run  persistence.UnrecordedRun
		want persistence.RunCostRecord
	}{
		{"part named for the reported model", spentRun("k", "m1"), persistence.RunCostRecord{
			EstimatedCostUSD: new(12.0), CostStatus: "estimated",
			NewParts:  map[string]persistence.ModelTokens{"m1": spent},
			PartCosts: map[string]persistence.ModelCost{"m1": estimated(12)},
		}},
		{"part named for the configured model without a reported one", spentRun("k", ""), persistence.RunCostRecord{
			EstimatedCostUSD: new(12.0), CostStatus: "estimated",
			NewParts:  map[string]persistence.ModelTokens{"m0": spent},
			PartCosts: map[string]persistence.ModelCost{"m0": estimated(12)},
		}},
		{"kind without an entry records unpriced run and part", spentRun("u", "m1"), persistence.RunCostRecord{
			CostStatus: "unpriced",
			NewParts:   map[string]persistence.ModelTokens{"m1": spent},
			PartCosts:  map[string]persistence.ModelCost{"m1": {CostStatus: "unpriced"}},
		}},
		{"spent nothing gets no part", measured, persistence.RunCostRecord{EstimatedCostUSD: new(0.0), CostStatus: "estimated"}},
		{"only total_tokens gets no part", totalOnly, persistence.RunCostRecord{CostStatus: "unpriced"}},
		{"parts price the run on its summed tokens and insert nothing", withParts, persistence.RunCostRecord{
			EstimatedCostUSD: new(15.0), CostStatus: "estimated",
			PartCosts: map[string]persistence.ModelCost{"a": estimated(1), "b": estimated(16)},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, want := recordJSON(recordedCostOf(tt.run, rates)), recordJSON(tt.want)

			if got != want {
				t.Errorf("recordedCostOf(%+v) = %s, want %s", tt.run, got, want)
			}
		})
	}
}

func recordJSON(record persistence.RunCostRecord) string {
	b, _ := json.Marshal(record)
	return string(b)
}
