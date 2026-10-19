package pricing

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
)

const priceTableReplaceFileCheck = "price_table.replace_file"

// Rates is the rate layers one workflow load put in force. It never changes
// after [LoadRates] returns it and is safe for concurrent use. A nil *Rates
// holds no layer and prices nothing.
type Rates struct {
	entries    TokenRates
	kinds      map[string]bool
	models     TokenRates
	layers     []tableLayer
	sameRow    []map[string][]string // per layer: table key -> model entry keys that resolve to it
	folded     map[string][]string   // fold of a model entry key -> the model entry keys with that fold
	version    string
	configured bool
}

// tableLayer is one price table in force and the source its rates are
// recorded under. Layers are ordered, highest precedence first.
type tableLayer struct {
	table  *Table
	source PriceSource
}

// LoadInput is what [LoadRates] needs beyond the configuration.
type LoadInput struct {
	// WorkflowDir resolves a relative price_table.replace_file.
	WorkflowDir string

	// IsKind reports whether a token_rates key names a registered agent
	// kind. It MUST NOT be nil.
	IsKind func(key string) bool

	// InForce is the rates of the last successful load, nil on the first.
	InForce *Rates
}

// Entry is one token_rates entry as in force.
type Entry struct {
	Key     string
	Rates   TokenRateConfig
	PerKind bool
}

// Complete reports whether the entry prices what it is meant to: a kind
// entry needs input and output set, a model entry needs every rate set.
func (e Entry) Complete() bool {
	if e.PerKind {
		return e.Rates.Complete()
	}
	return e.Rates.InputPerMtok != nil && e.Rates.OutputPerMtok != nil &&
		e.Rates.CacheReadPerMtok != nil && e.Rates.CacheWritePerMtok != nil
}

// SourceAvailable reports whether a table layer is in force or token_rates
// holds an entry, so some run can be priced.
func (r *Rates) SourceAvailable() bool {
	return r != nil && (len(r.entries) > 0 || len(r.layers) > 0)
}

// SourceConfigured reports whether the configuration names a rate source.
// It depends on the configuration alone, never on file content or other
// runtime state.
func (r *Rates) SourceConfigured() bool {
	return r != nil && r.configured
}

// Entries returns the token_rates entries in force sorted by key, nil when
// there is none.
func (r *Rates) Entries() []Entry {
	if r == nil || len(r.entries) == 0 {
		return nil
	}
	entries := make([]Entry, 0, len(r.entries))
	for _, key := range slices.Sorted(maps.Keys(r.entries)) {
		entries = append(entries, Entry{Key: key, Rates: r.entries[key], PerKind: r.kinds[key]})
	}
	return entries
}

// priceTableSettings is the price_table section after its defaults.
type priceTableSettings struct {
	embedded    bool
	replaceFile string
	replaceURL  string
}

// LoadRates puts the rate layers of cfg in force and returns them with the
// advisories the token_rates entries draw. A price_table that is malformed,
// or whose operator table cannot be read or parsed, fails the load; no
// failure falls back to another table.
//
// The operator table file, when set, is read once per call. LoadRates does
// no other I/O and no network access.
func LoadRates(cfg config.ServiceConfig, in LoadInput) (*Rates, []config.Advisory, error) {
	settings, err := readPriceTableSettings(cfg)
	if err != nil {
		return nil, nil, err
	}
	layers, err := loadLayers(settings, in)
	if err != nil {
		return nil, nil, err
	}

	rawRates, present := cfg.ExtensionValue("token_rates")
	entries, warnings := ParseTokenRates(rawRates, present)
	if in.InForce != nil {
		entries = reloadTokenRates(in.InForce.entries, rawRates, present)
	}

	r := &Rates{
		entries:    entries,
		kinds:      make(map[string]bool),
		models:     make(TokenRates),
		layers:     layers,
		version:    entries.Version(),
		configured: len(entries) > 0 || settings.replaceFile != "" || settings.embedded,
	}
	for key, rates := range entries {
		if in.IsKind(key) {
			r.kinds[key] = true
			continue
		}
		r.models[key] = rates
	}
	r.indexModels()
	return r, r.advisories(warnings), nil
}

func readPriceTableSettings(cfg config.ServiceConfig) (priceTableSettings, error) {
	settings := priceTableSettings{embedded: true}
	raw, _ := cfg.ExtensionValue("price_table")
	if raw == nil {
		return settings, nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return settings, &config.ConfigError{Field: "price_table", Message: fmt.Sprintf("expected map, got %T", raw)}
	}

	if v, set := section["embedded"]; set && v != nil {
		if settings.embedded, ok = v.(bool); !ok {
			return settings, &config.ConfigError{Field: "price_table.embedded", Message: fmt.Sprintf("expected boolean, got %T", v)}
		}
	}
	if v, set := section["replace_file"]; set && v != nil {
		if settings.replaceFile, ok = v.(string); !ok {
			return settings, &config.ConfigError{
				Field:   priceTableReplaceFileCheck,
				Message: fmt.Sprintf("expected string, got %T", v),
				Check:   priceTableReplaceFileCheck,
			}
		}
	}
	if v, set := section["replace_url"]; set && v != nil {
		if settings.replaceURL, ok = v.(string); !ok {
			return settings, &config.ConfigError{Field: "price_table.replace_url", Message: fmt.Sprintf("expected string, got %T", v)}
		}
	}

	if settings.replaceFile == "" && cfg.ExtensionEnvRefPaths("price_table")["replace_file"] {
		return settings, &config.ConfigError{
			Field:   priceTableReplaceFileCheck,
			Message: "resolved to empty (check environment variable)",
			Check:   priceTableReplaceFileCheck,
		}
	}
	if settings.replaceFile != "" && settings.replaceURL != "" {
		return settings, &config.ConfigError{
			Field:   "price_table",
			Message: "replace_file and replace_url are mutually exclusive",
			Check:   "price_table.replace_conflict",
		}
	}
	return settings, nil
}

func loadLayers(settings priceTableSettings, in LoadInput) ([]tableLayer, error) {
	if settings.replaceFile != "" {
		table, err := loadOperatorTable(settings.replaceFile, in)
		if err != nil {
			return nil, err
		}
		return []tableLayer{{table: table, source: SourceOperator}}, nil
	}
	if !settings.embedded {
		return nil, nil
	}
	table, err := EmbeddedTable()
	if err != nil {
		return nil, err
	}
	return []tableLayer{{table: table, source: SourceEmbedded}}, nil
}

func loadOperatorTable(replaceFile string, in LoadInput) (*Table, error) {
	path, err := config.ExpandPath(replaceFile, false)
	if err != nil {
		return nil, replaceFileError(fmt.Sprintf("%s: %v", replaceFile, err))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.WorkflowDir, path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // the operator names this table file in their own workflow
	if err != nil {
		return nil, replaceFileError(err.Error())
	}
	table, err := ParseTable(data, nil)
	if err != nil {
		return nil, replaceFileError(fmt.Sprintf("%s: %v", path, err))
	}
	if kept := in.InForce.operatorTable(table.Version()); kept != nil {
		return kept, nil
	}
	return table.withRevision(table.Version(), time.Now()), nil
}

func replaceFileError(message string) *config.ConfigError {
	return &config.ConfigError{Field: priceTableReplaceFileCheck, Message: message, Check: priceTableReplaceFileCheck}
}

// operatorTable returns the operator table in force when it has the given
// version, so its as-of time survives a reload of unchanged content.
func (r *Rates) operatorTable(version string) *Table {
	if r == nil {
		return nil
	}
	for _, layer := range r.layers {
		if layer.source == SourceOperator && layer.table.Version() == version {
			return layer.table
		}
	}
	return nil
}

func (r *Rates) indexModels() {
	r.sameRow = make([]map[string][]string, len(r.layers))
	for i, layer := range r.layers {
		r.sameRow[i] = make(map[string][]string)
		for key := range r.models {
			if row, ok := layer.table.Lookup(key); ok {
				r.sameRow[i][row.Key] = append(r.sameRow[i][row.Key], key)
			}
		}
	}
	r.folded = make(map[string][]string, len(r.models))
	for key := range r.models {
		r.folded[fold(key)] = append(r.folded[fold(key)], key)
	}
}

func (r *Rates) advisories(parseWarnings []string) []config.Advisory {
	var advisories []config.Advisory
	for _, w := range parseWarnings {
		advisories = append(advisories, tokenRatesAdvisory("token_rates", "skipped invalid token rate entry", w))
	}

	keys := slices.Sorted(maps.Keys(r.entries))
	for _, key := range keys {
		if r.kinds[key] && !r.entries[key].Complete() {
			advisories = append(advisories, tokenRatesAdvisory("token_rates", "skipped invalid token rate entry",
				fmt.Sprintf("token_rates.%s: entry needs both input_per_mtok and output_per_mtok and prices nothing", key)))
		}
	}
	for _, key := range keys {
		if r.kinds[key] {
			advisories = append(advisories, tokenRatesAdvisory("token_rates.per_kind", "token rate entry turns off per-model pricing",
				fmt.Sprintf("token_rates.%s: prices every model of agent kind %s alike, which turns off per-model pricing for that kind", key, key)))
		}
	}
	for _, key := range keys {
		if r.kinds[key] || r.listedByLayer(key) {
			continue
		}
		if unset := unsetRateKeys(r.entries[key]); len(unset) > 0 {
			advisories = append(advisories, tokenRatesAdvisory("token_rates.model_incomplete", "token rate entry for an unlisted model is incomplete",
				fmt.Sprintf("token_rates.%s: no price table lists this model, so the entry should set input_per_mtok, output_per_mtok, cache_read_per_mtok, and cache_write_per_mtok; it prices only the classes it sets; not set: %s",
					key, strings.Join(unset, ", "))))
		}
	}
	return advisories
}

func tokenRatesAdvisory(check, message, text string) config.Advisory {
	return config.Advisory{
		Check:   check,
		Text:    text,
		Message: message,
		Attrs:   []slog.Attr{slog.String("detail", text)},
	}
}

func (r *Rates) listedByLayer(model string) bool {
	for _, layer := range r.layers {
		if _, ok := layer.table.Lookup(model); ok {
			return true
		}
	}
	return false
}

// unsetRateKeys names the rates cfg leaves unset, in the order rateKeys
// lists them.
func unsetRateKeys(cfg TokenRateConfig) []string {
	set := [...]*float64{cfg.InputPerMtok, cfg.OutputPerMtok, cfg.CacheReadPerMtok, cfg.CacheWritePerMtok}
	var unset []string
	for i, rate := range set {
		if rate == nil {
			unset = append(unset, rateKeys[i])
		}
	}
	return unset
}

// resolution is the rate per token of each class that prices one model's
// tokens, and the source and version recorded with the price.
type resolution struct {
	perToken [rateClasses]*float64
	source   PriceSource
	version  string
}

func (res *resolution) pricesAnything() bool {
	return res != nil && slices.ContainsFunc(res.perToken[:], func(rate *float64) bool { return rate != nil })
}

// resolve returns the rates that price model, or nil when neither a model
// entry nor a table layer has any.
func (r *Rates) resolve(model string) *resolution {
	if r == nil {
		return nil
	}
	var (
		row    TableEntry
		layer  int
		listed bool
	)
	for i, candidate := range r.layers {
		if entry, ok := candidate.table.Lookup(model); ok {
			row, layer, listed = entry, i, true
			break
		}
	}

	operator, hasOperator := r.operatorModelEntry(model, layer, row, listed)
	switch {
	case !hasOperator && !listed:
		return nil
	case !hasOperator:
		return &resolution{
			perToken: [rateClasses]*float64{row.InputPerToken, row.OutputPerToken, row.CacheReadPerToken, row.CacheWritePerToken},
			source:   r.layers[layer].source,
			version:  r.layers[layer].table.Version(),
		}
	}

	tableRates := [rateClasses]*float64{row.InputPerToken, row.OutputPerToken, row.CacheReadPerToken, row.CacheWritePerToken}
	operatorRates := [rateClasses]*float64{
		perToken(operator.InputPerMtok), perToken(operator.OutputPerMtok),
		perToken(operator.CacheReadPerMtok), perToken(operator.CacheWritePerMtok),
	}
	res := &resolution{source: SourceOperator, version: r.version}
	takesTableRate := false
	for class := range rateClasses {
		switch {
		case operatorRates[class] != nil:
			res.perToken[class] = operatorRates[class]
		case tableRates[class] != nil:
			res.perToken[class] = tableRates[class]
			takesTableRate = true
		}
	}
	if takesTableRate {
		res.version += "+" + r.layers[layer].table.Version()
	}
	return res
}

func perToken(perMtok *float64) *float64 {
	if perMtok == nil {
		return nil
	}
	rate := *perMtok / 1_000_000
	return &rate
}

// operatorModelEntry finds the model entry that applies to model: the one
// keyed exactly so, else the one that maps to the same table entry, else,
// for a name no layer lists, the one equal under the matching fold. Two
// candidates at one step apply to nothing.
func (r *Rates) operatorModelEntry(model string, layer int, row TableEntry, listed bool) (TokenRateConfig, bool) {
	if rates, ok := r.models[model]; ok {
		return rates, true
	}
	var candidates []string
	if listed {
		candidates = r.sameRow[layer][row.Key]
	} else {
		candidates = r.folded[fold(model)]
	}
	if len(candidates) != 1 {
		return TokenRateConfig{}, false
	}
	return r.models[candidates[0]], true
}
