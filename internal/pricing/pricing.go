// Package pricing parses the token_rates workflow section, reports its
// advisories, and estimates the USD cost of token usage under a per-kind
// rate. [ParseTokenRates] and [EstimateCost] are the entry points;
// [TokenRateConfig.Complete] states which entries price anything.
package pricing

import (
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// rateKeys lists the four recognized token-rate keys, in the order
// ParseTokenRates checks them within one kind's entry.
var rateKeys = []string{"input_per_mtok", "output_per_mtok", "cache_read_per_mtok", "cache_write_per_mtok"}

// TokenRateConfig holds per-token-type USD rates for cost estimation.
// All rates are in USD per 1 million tokens (per-mtok). A nil pointer
// indicates the rate is not configured; cost estimation is suppressed
// for that token type.
type TokenRateConfig struct {
	InputPerMtok      *float64
	OutputPerMtok     *float64
	CacheReadPerMtok  *float64
	CacheWritePerMtok *float64
}

// Complete reports whether the entry sets both InputPerMtok and
// OutputPerMtok. An entry without both rates prices nothing.
func (c TokenRateConfig) Complete() bool {
	return c.InputPerMtok != nil && c.OutputPerMtok != nil
}

// TokenRates maps agent adapter kind strings to their token rate
// configuration. A nil or empty map means no cost estimates are made.
type TokenRates map[string]TokenRateConfig

// ParseTokenRates extracts and validates token rates from the raw
// "token_rates" extension value. present tells whether the key
// existed in the extensions map at all, distinguishing an absent key
// from one holding a nil value. Returns nil rates when the key is
// absent or empty. Warnings are advisory and do not prevent boot.
func ParseTokenRates(rawSection any, present bool) (TokenRates, []string) {
	if !present || rawSection == nil {
		return nil, nil
	}

	topMap, ok := rawSection.(map[string]any)
	if !ok {
		return nil, []string{fmt.Sprintf("token_rates: expected map, got %T", rawSection)}
	}
	if len(topMap) == 0 {
		return nil, nil
	}

	var warnings []string
	rates := make(TokenRates, len(topMap))

	for _, kind := range slices.Sorted(maps.Keys(topMap)) {
		val := topMap[kind]
		if kind == "" {
			warnings = append(warnings, "token_rates: entry keyed to the empty string is dropped")
			continue
		}

		var cfg TokenRateConfig
		kindMap, ok := val.(map[string]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("token_rates.%s: expected map, got %T", kind, val))
			rates[kind] = cfg
			continue
		}

		if v, w := extractRate(kindMap, "input_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.InputPerMtok = v
		}
		if v, w := extractRate(kindMap, "output_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.OutputPerMtok = v
		}
		if v, w := extractRate(kindMap, "cache_read_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.CacheReadPerMtok = v
		}
		if v, w := extractRate(kindMap, "cache_write_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.CacheWritePerMtok = v
		}

		for _, key := range slices.Sorted(maps.Keys(kindMap)) {
			if slices.Contains(rateKeys, key) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("token_rates.%s.%s: unrecognized key is ignored", kind, key))
		}

		if !cfg.Complete() {
			warnings = append(warnings,
				fmt.Sprintf("token_rates.%s: entry needs both input_per_mtok and output_per_mtok and prices nothing", kind))
		}

		rates[kind] = cfg
	}

	if len(rates) == 0 {
		return nil, warnings
	}
	return rates, warnings
}

// TokenRateAdvisories runs ParseTokenRates over cfg's "token_rates"
// extension value and returns one [config.Advisory] per warning, in
// warning order. It performs no I/O and no mutation.
func TokenRateAdvisories(cfg config.ServiceConfig) []config.Advisory {
	rawTokenRates, present := cfg.ExtensionValue("token_rates")
	_, warnings := ParseTokenRates(rawTokenRates, present)

	var advisories []config.Advisory
	for _, w := range warnings {
		advisories = append(advisories, config.Advisory{
			Check:   "token_rates",
			Text:    w,
			Message: "skipped invalid token rate entry",
			Attrs:   []slog.Attr{slog.String("detail", w)},
		})
	}
	return advisories
}

// extractRate reads a non-negative, finite float64 from a map entry.
// Returns nil with empty warning when the key is absent, nil with a
// warning when the value is invalid.
func extractRate(m map[string]any, key, kind string) (*float64, string) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, ""
	}

	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	default:
		return nil, fmt.Sprintf("token_rates.%s.%s: expected number, got %T", kind, key, raw)
	}

	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Sprintf("token_rates.%s.%s: rate %v is not finite", kind, key, f)
	}
	if f < 0 {
		return nil, fmt.Sprintf("token_rates.%s.%s: negative rate %v", kind, key, f)
	}
	return &f, ""
}

// EstimateCost prices usage under the disjoint-bucket rule: fresh
// input, cache reads, cache writes, and output are each priced once,
// at their own rate. An unset cache-read or cache-write rate prices
// that class at the input rate. Returns nil when rates is nil or
// lacks InputPerMtok or OutputPerMtok; it never reads usage.TotalTokens.
func EstimateCost(usage domain.TokenUsage, rates *TokenRateConfig) *float64 {
	if rates == nil || !rates.Complete() {
		return nil
	}

	readRate := *rates.InputPerMtok
	if rates.CacheReadPerMtok != nil {
		readRate = *rates.CacheReadPerMtok
	}
	writeRate := *rates.InputPerMtok
	if rates.CacheWritePerMtok != nil {
		writeRate = *rates.CacheWritePerMtok
	}

	fresh := max(usage.InputTokens-usage.CacheReadTokens-usage.CacheWriteTokens, 0)
	cost := (float64(fresh)*(*rates.InputPerMtok) +
		float64(usage.CacheReadTokens)*readRate +
		float64(usage.CacheWriteTokens)*writeRate +
		float64(usage.OutputTokens)*(*rates.OutputPerMtok)) / 1_000_000
	return &cost
}
