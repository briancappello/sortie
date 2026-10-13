package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/server"
)

type runRef struct {
	RunID int64 `json:"run_id"`
}

type runsHarness struct {
	store *persistence.Store
	url   string
}

var claudeRates = server.TokenRateConfig{InputPerMtok: new(3.0), OutputPerMtok: new(15.0), CacheReadPerMtok: new(0.3), CacheWritePerMtok: new(3.75)}

func newRunsHarness(t *testing.T, snap orchestrator.RuntimeSnapshotResult) *runsHarness {
	t.Helper()
	store, err := persistence.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	srv := server.New(server.Params{
		SnapshotFn: func() (orchestrator.RuntimeSnapshotResult, error) { return snap, nil },
		RefreshFn:  func() bool { return true },
		TokenRates: server.TokenRates{"claude-code": claudeRates},
		RunHistory: runHistoryReader{store: store},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &runsHarness{store: store, url: ts.URL}
}

func (h *runsHarness) append(t *testing.T, run persistence.RunHistory) int64 {
	t.Helper()
	got, err := h.store.AppendRunHistory(t.Context(), run)
	if err != nil {
		t.Fatalf("AppendRunHistory(%q): %v", run.Identifier, err)
	}
	return got.ID
}

func (h *runsHarness) get(t *testing.T, route string, out any) int {
	t.Helper()
	resp, err := http.Get(h.url + route)
	if err != nil {
		t.Fatalf("GET %s: %v", route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_ = json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode
}

func (h *runsHarness) drain(t *testing.T, query string, afterFirstPage func()) (ids []int64, moreAfter []bool) {
	t.Helper()
	cursor := ""
	for range 10 {
		var body struct {
			Runs       []runRef `json:"runs"`
			NextCursor *string  `json:"next_cursor"`
		}
		route := "/api/v1/runs?" + query + cursor
		if status := h.get(t, route, &body); status != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", route, status, http.StatusOK)
		}
		for _, run := range body.Runs {
			ids = append(ids, run.RunID)
		}
		moreAfter = append(moreAfter, body.NextCursor != nil)
		if body.NextCursor == nil {
			return ids, moreAfter
		}
		cursor = "&cursor=" + *body.NextCursor
		if afterFirstPage != nil && len(moreAfter) == 1 {
			afterFirstPage()
		}
	}
	t.Fatalf("GET /api/v1/runs?%s: next_cursor never ended", query)
	return nil, nil
}

func runRow(issueID, identifier string) persistence.RunHistory {
	return persistence.RunHistory{IssueID: issueID, Identifier: identifier, AgentAdapter: "claude-code", Status: "succeeded", RuleName: "implement", CompletedAt: "2026-03-01T09:00:00Z", TokensMeasured: true}
}

func TestRunHistoryPagesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()

	h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{})
	var newestFirst []int64
	for range 5 {
		newestFirst = slices.Insert(newestFirst, 0, h.append(t, runRow("i-0", "OTHER-0")))
	}
	var appended int64

	ids, moreAfter := h.drain(t, "limit=2", func() { appended = h.append(t, runRow("i-0", "OTHER-0")) })
	fresh, _ := h.drain(t, "limit=2", nil)

	if got, want := fmt.Sprint(ids, moreAfter), fmt.Sprint(newestFirst, []bool{true, true, false}); got != want {
		t.Errorf("run IDs and next_cursor set per page = %s, want %s", got, want)
	}
	if fresh[0] != appended {
		t.Errorf("fresh first page starts with run %d, want %d", fresh[0], appended)
	}
}

func TestRunHistoryFilters(t *testing.T) {
	t.Parallel()

	const ten, twelve = "2026-03-01T10:00:00Z", "2026-03-01T12:00:00Z"
	tests := []struct {
		name, query, field, hit, miss string
	}{
		{"issue", "issue=PROJ-1", "Identifier", "PROJ-1", "PROJ-9"},
		{"issue_id", "issue_id=i-1", "IssueID", "i-1", "i-9"},
		{"rule", "rule=review", "RuleName", "review", "implement"},
		{"rule none", "rule=%3Cnone%3E", "RuleName", "", "implement"},
		{"status", "status=failed", "Status", "failed", "succeeded"},
		{"stage", "stage=review", "StageTarget", "review", "deploy"},
		{"since includes its bound", "since=" + ten, "CompletedAt", ten, "2026-03-01T09:59:59Z"},
		{"until excludes its bound", "until=" + twelve, "CompletedAt", "2026-03-01T11:59:59Z", twelve},
		{"since with until", "since=" + ten + "&until=" + twelve, "CompletedAt", ten, twelve},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{})
			seed := func(value string) int64 {
				run := runRow("i-0", "OTHER-0")
				reflect.ValueOf(&run).Elem().FieldByName(tt.field).SetString(value)
				return h.append(t, run)
			}
			seed(tt.miss)
			older := seed(tt.hit)
			newer := seed(tt.hit)
			seed(tt.miss)
			seed(tt.miss)

			ids, moreAfter := h.drain(t, tt.query+"&limit=1", nil)

			if got, want := fmt.Sprint(ids, moreAfter), fmt.Sprint([]int64{newer, older}, []bool{true, false}); got != want {
				t.Errorf("GET /api/v1/runs?%s&limit=1 run IDs and next_cursor set per page = %s, want %s", tt.query, got, want)
			}
		})
	}
}

func TestRunHistoryServesCostAndOrigin(t *testing.T) {
	t.Parallel()

	cost := server.EstimateCost(domain.TokenUsage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, CacheWriteTokens: 100}, &claudeRates)
	tokens := func(input, output, total, cacheRead, cacheWrite any) map[string]any {
		return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": total, "cache_read_tokens": cacheRead, "cache_write_tokens": cacheWrite}
	}
	measured := runRow("i-0", "OTHER-0")
	measured.Trigger, measured.ReactionKind, measured.InputTokens, measured.OutputTokens, measured.TotalTokens, measured.CacheReadTokens, measured.CacheWriteTokens = new("ci_fix"), "ci", 1000, 500, 1500, 200, 100
	unmeasured := runRow("i-0", "OTHER-0")
	unmeasured.TokensMeasured = false

	h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{})
	h.append(t, measured)
	h.append(t, unmeasured)
	var body struct{ Runs []map[string]any }
	if status := h.get(t, "/api/v1/runs", &body); status != http.StatusOK || len(body.Runs) != 2 {
		t.Fatalf("GET /api/v1/runs = %d with %d runs, want %d with 2", status, len(body.Runs), http.StatusOK)
	}

	wants := []map[string]any{
		{"tokens": tokens(nil, nil, nil, nil, nil), "estimated_cost_usd": nil, "cost_status": "unmeasured", "trigger": nil, "reaction_kind": "", "reaction_config_key": ""},
		{"tokens": tokens(1000.0, 500.0, 1500.0, 200.0, 100.0), "estimated_cost_usd": *cost, "cost_status": "estimated", "trigger": "ci_fix", "reaction_kind": "ci", "reaction_config_key": "ci_failure"},
	}
	for i, want := range wants {
		for member, wantValue := range want {
			if got := body.Runs[i][member]; !reflect.DeepEqual(got, wantValue) {
				t.Errorf("GET /api/v1/runs runs[%d].%s = %v, want %v", i, member, got, wantValue)
			}
		}
	}
}

func TestRunHistoryFinishedIssueResolvesByOlderIdentifier(t *testing.T) {
	t.Parallel()

	running := orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{{IssueID: "i-1", Identifier: "PROJ-2", IssueTitle: "Fix login"}}}
	tests := []struct {
		name, route, want string
		snap              orchestrator.RuntimeSnapshotResult
	}{
		{name: "nothing live", route: "/api/v1/PROJ-1", want: "200|history||PROJ-2|Fix login"},
		{name: "live under the newer identifier", route: "/api/v1/PROJ-1", snap: running, want: "200|running||PROJ-2|Fix login"},
		{name: "no run and no live entry", route: "/api/v1/NOPE-1", want: "404||issue_not_found||"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunsHarness(t, tt.snap)
			hopRun := runRow("i-1", "PROJ-1")
			hopRun.StageTarget, hopRun.StageResult = "review", "advanced"
			newestRun := runRow("i-1", "PROJ-2")
			newestRun.IssueTitle = "Fix login"
			oldest := h.append(t, runRow("i-1", "PROJ-1"))
			h.append(t, runRow("i-2", "PROJ-7"))
			hop := h.append(t, hopRun)
			newest := h.append(t, newestRun)

			var got struct {
				IssueIdentifier string `json:"issue_identifier"`
				Status          string
				Issue           struct{ Title string }
				History         *struct {
					Runs   []runRef
					Totals struct {
						RunCount int `json:"run_count"`
					}
					Hops []struct {
						runRef
						TargetRule string `json:"target_rule"`
					}
				}
				Error struct{ Code string }
			}
			status := h.get(t, tt.route, &got)

			gotSummary := strings.Join([]string{strconv.Itoa(status), got.Status, got.Error.Code, got.IssueIdentifier, got.Issue.Title}, "|")
			if gotSummary != tt.want {
				t.Fatalf("GET %s status|status|error|issue_identifier|title = %q, want %q", tt.route, gotSummary, tt.want)
			}
			if status == http.StatusNotFound {
				return
			}
			gotHistory := fmt.Sprint(got.History.Runs, got.History.Totals.RunCount, got.History.Hops)
			if wantHistory := fmt.Sprintf("[{%d} {%d} {%d}] 3 [{{%d} review}]", newest, hop, oldest, hop); gotHistory != wantHistory {
				t.Errorf("GET %s history runs, run_count, hops = %s, want %s", tt.route, gotHistory, wantHistory)
			}
		})
	}
}
