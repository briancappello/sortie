package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/persistence"
)

const goldenStatsDir = "testdata/golden_stats"

const goldenStatsTokenRates = `token_rates:
  claude-code:
    input_per_mtok: 3
    output_per_mtok: 15
    cache_read_per_mtok: 0.3
    cache_write_per_mtok: 3.75
`

func goldenStatsRuns() []persistence.RunHistory {
	const (
		start   = "2026-01-10T10:00:00Z"
		ranFor  = "2026-01-10T10:10:00Z"
		ranLong = "2026-01-10T10:30:00Z"
	)
	runs := []persistence.RunHistory{
		{
			AgentAdapter: "claude-code", Status: "succeeded", TurnsCompleted: 3, CompletedAt: ranFor,
			RuleName: "implement", TemplateID: "tpl-a", ConfiguredModel: "claude-sonnet",
			InputTokens: 1_000_000, OutputTokens: 200_000, TotalTokens: 1_200_000, CacheReadTokens: 50_000, CacheWriteTokens: 10_000, TokensMeasured: true,
			ChainID: "chain-1", StageTarget: "review", StageResult: "advanced",
			ReviewMetadata: new(`{"enabled":true,"total_iterations":2,"final_verdict":"pass","cap_reached":false}`),
		},
		{
			AgentAdapter: "claude-code", Status: "succeeded", TurnsCompleted: 5, CompletedAt: ranLong,
			RuleName: "review", TemplateID: "tpl-b", ConfiguredModel: "claude-opus",
			InputTokens: 2_000_000, OutputTokens: 400_000, TotalTokens: 2_400_000, TokensMeasured: true,
			ChainID: "chain-1", StagePrevious: "implement", StagePreviousOutcome: "advanced",
			ReviewMetadata: new(`{"enabled":true,"total_iterations":3,"final_verdict":"iterate","cap_reached":true}`),
		},
		{
			AgentAdapter: "claude-code", Status: "failed", TurnsCompleted: 2, CompletedAt: "2026-01-10T10:05:00Z",
			RuleName: "implement", TemplateID: "tpl-a", ConfiguredModel: "claude-sonnet",
			InputTokens: 300_000, OutputTokens: 50_000, TotalTokens: 350_000, TokensMeasured: true,
			ChainID: "chain-2", StageResult: "failed",
		},
		{
			AgentAdapter: "claude-code", Status: "ci_failed", CompletedAt: start,
			InputTokens: 100_000, OutputTokens: 10_000, TotalTokens: 110_000, TokensMeasured: true,
		},
		{
			AgentAdapter: "codex", Status: "succeeded", TurnsCompleted: 4, CompletedAt: "2026-01-10T10:20:00Z",
			RuleName: "implement", ConfiguredModel: "gpt-5",
			InputTokens: 500_000, OutputTokens: 100_000, TotalTokens: 600_000, TokensMeasured: true,
			ReviewMetadata: new(`not json`),
		},
		{
			Status: "failed", TurnsCompleted: 1, StartedAt: "yesterday", CompletedAt: ranFor,
		},
	}
	for i := range runs {
		runs[i].IssueID = fmt.Sprintf("ISS-%d", i+1)
		runs[i].Identifier = fmt.Sprintf("PROJ-%d", i+1)
		runs[i].Attempt = 1
		runs[i].Workspace = "/tmp/ws"
		if runs[i].StartedAt == "" {
			runs[i].StartedAt = start
		}
	}
	return runs
}

func jsonEncodedText(t *testing.T, s string) string {
	t.Helper()

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(%q): %v", s, err)
	}
	return string(b[1 : len(b)-1])
}

func checkGoldenStats(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join(goldenStatsDir, name)
	if *recordGolden {
		if err := os.MkdirAll(goldenStatsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", goldenStatsDir, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden fixture %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %s differs\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestRunStatsMatchesGolden(t *testing.T) {
	pinStatsNow(t, time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC))

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(goldenStatsTokenRates))
	dbPath := filepath.Join(dir, ".sortie.db")
	createStatsDB(t, dbPath, goldenStatsRuns()...)

	rangeArgs := []string{"--since", "2026-01-01T00:00:00Z", "--until", "2026-02-01T00:00:00Z", wfPath}
	tests := []struct {
		name   string
		golden string
		args   []string
		wf, db string
	}{
		{"json", "report.json", append([]string{"stats", "--format", "json"}, rangeArgs...), jsonEncodedText(t, wfPath), jsonEncodedText(t, dbPath)},
		{"text", "report.txt", append([]string{"stats"}, rangeArgs...), wfPath, dbPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(context.Background(), tt.args, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("run(%v) = %d, want 0; stderr: %s", tt.args, code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%v) stderr = %q, want empty", tt.args, stderr.String())
			}
			normalized := strings.NewReplacer(tt.wf, "WORKFLOW_PATH", tt.db, "DB_PATH").Replace(stdout.String())
			checkGoldenStats(t, tt.golden, []byte(normalized))
		})
	}
}
