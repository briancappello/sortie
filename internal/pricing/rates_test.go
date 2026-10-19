package pricing

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
)

const listedTable = `{
  "listed-model": {"litellm_provider": "acme", "input_cost_per_token": 0.000001, "output_cost_per_token": 0.000005, "cache_read_input_token_cost": 0.0000005, "cache_creation_input_token_cost": 0.000002}
}`

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir
}

func operatorTable() map[string]any { return map[string]any{"replace_file": "prices.json"} }

func noTables() map[string]any { return map[string]any{"embedded": false} }

func perMtok(input, output float64) map[string]any {
	return map[string]any{"input_per_mtok": input, "output_per_mtok": output}
}

func fullPerMtok(input, output, cacheRead, cacheWrite float64) map[string]any {
	return map[string]any{"input_per_mtok": input, "output_per_mtok": output, "cache_read_per_mtok": cacheRead, "cache_write_per_mtok": cacheWrite}
}

func kindsOf(keys ...string) func(string) bool {
	return func(key string) bool { return slices.Contains(keys, key) }
}

func loadRates(t *testing.T, ext map[string]any, in LoadInput) (*Rates, []config.Advisory, error) {
	t.Helper()

	cfg, err := config.NewServiceConfig(ext)
	if err != nil {
		t.Fatalf("NewServiceConfig(%v) = %v, want nil", ext, err)
	}
	return LoadRates(cfg, in)
}

func mustLoadRates(t *testing.T, ext map[string]any, kinds ...string) *Rates {
	t.Helper()

	in := LoadInput{WorkflowDir: writeFiles(t, map[string]string{"prices.json": listedTable}), IsKind: kindsOf(kinds...)}
	rates, _, err := loadRates(t, ext, in)
	if err != nil {
		t.Fatalf("LoadRates(%v) = %v, want nil", ext, err)
	}
	return rates
}

func TestLoadRates(t *testing.T) {
	t.Parallel()

	files := map[string]string{"prices.json": listedTable}

	tests := []struct {
		name           string
		ext            map[string]any
		files          map[string]string
		kinds          []string
		prior          map[string]any
		wantCheck      string
		wantAdvisories []string
		wantNotSet     string
		wantEntries    []string
		wantAvailable  bool
		wantConfigured bool
	}{
		{
			name:           "defaults put the embedded table in force",
			ext:            map[string]any{},
			wantAvailable:  true,
			wantConfigured: true,
		},
		{
			name: "embedded disabled with no other source",
			ext:  map[string]any{"price_table": noTables()},
		},
		{
			name:           "embedded disabled beside a kind entry",
			ext:            map[string]any{"price_table": noTables(), "token_rates": map[string]any{"k": perMtok(3, 15)}},
			kinds:          []string{"k"},
			wantAdvisories: []string{"token_rates.per_kind"},
			wantEntries:    []string{"k kind=true complete=true"},
			wantAvailable:  true,
			wantConfigured: true,
		},
		{
			name:           "kind and model entries differ in completeness and are sorted",
			ext:            map[string]any{"token_rates": map[string]any{"zeta": perMtok(3, 15), "mid": perMtok(3, 15), "alpha": fullPerMtok(3, 15, 0.3, 6)}},
			kinds:          []string{"zeta"},
			wantAdvisories: []string{"token_rates.per_kind", "token_rates.model_incomplete"},
			wantNotSet:     "cache_read_per_mtok, cache_write_per_mtok",
			wantEntries:    []string{"alpha kind=false complete=true", "mid kind=false complete=false", "zeta kind=true complete=true"},
			wantAvailable:  true,
			wantConfigured: true,
		},
		{
			name: "incomplete kind entry and incomplete model entry draw their own advisories in order",
			ext: map[string]any{"token_rates": map[string]any{
				"k": map[string]any{"input_per_mtok": 3.0},
				"m": map[string]any{"output_per_mtok": 15.0},
			}},
			kinds:          []string{"k"},
			wantAdvisories: []string{"token_rates", "token_rates.per_kind", "token_rates.model_incomplete"},
			wantNotSet:     "input_per_mtok, cache_read_per_mtok, cache_write_per_mtok",
			wantEntries:    []string{"k kind=true complete=false", "m kind=false complete=false"},
			wantAvailable:  true,
			wantConfigured: true,
		},
		{
			name:           "model entry for a model the operator table lists loads quietly",
			ext:            map[string]any{"price_table": operatorTable(), "token_rates": map[string]any{"listed-model": map[string]any{"input_per_mtok": 3.0}}},
			files:          files,
			wantEntries:    []string{"listed-model kind=false complete=false"},
			wantAvailable:  true,
			wantConfigured: true,
		},
		{
			name:           "invalid rate on reload keeps the model entry in force",
			ext:            map[string]any{"token_rates": map[string]any{"m": map[string]any{"input_per_mtok": -1.0, "output_per_mtok": 1.0}}},
			prior:          map[string]any{"token_rates": map[string]any{"m": fullPerMtok(3, 15, 0.3, 6)}},
			wantAdvisories: []string{"token_rates"},
			wantEntries:    []string{"m kind=false complete=true"},
			wantAvailable:  true,
			wantConfigured: true,
		},
		{
			name:      "price_table that is not a map",
			ext:       map[string]any{"price_table": "oops"},
			wantCheck: "config.price_table",
		},
		{
			name:      "replace_file that is not a string",
			ext:       map[string]any{"price_table": map[string]any{"replace_file": 7}},
			wantCheck: "price_table.replace_file",
		},
		{
			name:      "replace_file beside replace_url",
			ext:       map[string]any{"price_table": map[string]any{"replace_file": "prices.json", "replace_url": "https://prices.test/table.json"}},
			files:     files,
			wantCheck: "price_table.replace_conflict",
		},
		{
			name:      "replace_file naming a missing file",
			ext:       map[string]any{"price_table": operatorTable()},
			wantCheck: "price_table.replace_file",
		},
		{
			name:      "replace_file naming a table no entry of which carries a rate",
			ext:       map[string]any{"price_table": operatorTable()},
			files:     map[string]string{"prices.json": `{"empty": {"litellm_provider": "acme"}}`},
			wantCheck: "price_table.replace_file",
		},
		{
			name:      "replace_file variable that resolves to an empty string",
			ext:       map[string]any{"price_table": map[string]any{"replace_file": "$SORTIE_TEST_PRICING_UNSET_REPLACE_FILE"}},
			wantCheck: "price_table.replace_file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := LoadInput{WorkflowDir: writeFiles(t, tt.files), IsKind: kindsOf(tt.kinds...)}
			if tt.prior != nil {
				in.InForce = mustLoadRates(t, tt.prior, tt.kinds...)
			}

			rates, advisories, err := loadRates(t, tt.ext, in)

			if tt.wantCheck != "" {
				var ce *config.ConfigError
				if !errors.As(err, &ce) {
					t.Fatalf("LoadRates(%v) error = %v, want *config.ConfigError", tt.ext, err)
				}
				if got := cmp.Or(ce.Check, "config."+ce.Field); got != tt.wantCheck {
					t.Errorf("LoadRates(%v) check = %q, want %q", tt.ext, got, tt.wantCheck)
				}
				if rates != nil || advisories != nil {
					t.Errorf("LoadRates(%v) = %v, %v, want nil rates and advisories on error", tt.ext, rates, advisories)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadRates(%v) = %v, want nil", tt.ext, err)
			}

			var gotChecks []string
			for _, adv := range advisories {
				gotChecks = append(gotChecks, adv.Check)
				if len(adv.Attrs) != 1 || adv.Attrs[0].Key != "detail" || adv.Attrs[0].Value.String() != adv.Text {
					t.Errorf("advisory %q Attrs = %v, want one detail attribute holding its text", adv.Check, adv.Attrs)
				}
				if adv.Check == "token_rates.model_incomplete" && !strings.Contains(adv.Text, "not set: "+tt.wantNotSet) {
					t.Errorf("advisory %q text = %q, want it to end with the unset rates %q", adv.Check, adv.Text, tt.wantNotSet)
				}
			}
			if !slices.Equal(gotChecks, tt.wantAdvisories) {
				t.Errorf("LoadRates(%v) advisory checks = %v, want %v", tt.ext, gotChecks, tt.wantAdvisories)
			}

			var gotEntries []string
			for _, e := range rates.Entries() {
				gotEntries = append(gotEntries, fmt.Sprintf("%s kind=%t complete=%t", e.Key, e.PerKind, e.Complete()))
			}
			if !slices.Equal(gotEntries, tt.wantEntries) {
				t.Errorf("LoadRates(%v).Entries() = %q, want %q", tt.ext, gotEntries, tt.wantEntries)
			}
			if rates.SourceAvailable() != tt.wantAvailable || rates.SourceConfigured() != tt.wantConfigured {
				t.Errorf("LoadRates(%v) SourceAvailable, SourceConfigured = %t, %t, want %t, %t",
					tt.ext, rates.SourceAvailable(), rates.SourceConfigured(), tt.wantAvailable, tt.wantConfigured)
			}
		})
	}
}

func TestLoadRates_ReloadKeepsOperatorTableOfUnchangedContent(t *testing.T) {
	t.Parallel()

	ext := map[string]any{"price_table": operatorTable()}
	in := LoadInput{WorkflowDir: writeFiles(t, map[string]string{"prices.json": listedTable}), IsKind: kindsOf()}

	first, _, err := loadRates(t, ext, in)
	if err != nil {
		t.Fatalf("LoadRates() = %v, want nil", err)
	}
	in.InForce = first
	again, _, err := loadRates(t, ext, in)
	if err != nil {
		t.Fatalf("LoadRates() on unchanged content = %v, want nil", err)
	}

	if again.layers[0].table != first.layers[0].table || first.layers[0].table.AsOf().IsZero() {
		t.Errorf("LoadRates() on unchanged content replaced the operator table or lost its as-of time %v", first.layers[0].table.AsOf())
	}
}
