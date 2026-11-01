package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/plan"
	"github.com/sortie-ai/sortie/internal/registry"
)

type rowsLoader map[string][]persistence.PlanSatisfaction

func (r rowsLoader) LoadPlanSatisfaction(_ context.Context, ids []string) (map[string][]persistence.PlanSatisfaction, error) {
	out := map[string][]persistence.PlanSatisfaction{}
	for _, id := range ids {
		if rows, ok := r[id]; ok {
			out[id] = rows
		}
	}
	return out, nil
}

func planControlGraphServer(t *testing.T, tr *planTracker, rows rowsLoader, on bool) *httptest.Server {
	t.Helper()
	states := domain.TrackerStates{}
	if on {
		states = domain.TrackerStates{Release: "Todo", Backlog: "Backlog", Cancel: "Canceled", Done: "Done"}
	}
	srv := New(Params{
		SnapshotFn:   fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:    acceptingRefresh(),
		Logger:       slog.New(slog.DiscardHandler),
		ControlToken: planTestToken,
		PlanProjects: plan.NewProjects("T", plan.NewSyncer(plan.Tracker{Adapter: tr, Meta: registry.TrackerMeta{
			WriteCapabilities: registry.AllWriteCapabilities(), PlanKeyStorage: registry.PlanKeyCustomField,
		}}), nil),
		TerminalStatesFn: func() []string { return []string{"Done", "Canceled"} },
		PlanStatesFn:     func() domain.TrackerStates { return states },
		PlanSatisfaction: rows,
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func graphTracker() *planTracker {
	return &planTracker{issues: []domain.PlanIssue{
		{ID: "a3", Identifier: "T-3", PlanKey: "billing/a3", Kind: domain.IssueKindStory, State: "In Review", BlockedBy: []string{},
			Labels: []string{"sortie-review:human"}, Approval: &domain.PlanApproval{By: "Ana", ByID: "iss|ana", At: "2026-10-16T14:02:00Z"}},
		{ID: "a4", Identifier: "T-4", PlanKey: "billing/a4", Kind: domain.IssueKindStory, State: "Canceled", BlockedBy: []string{}},
		{ID: "b1", Identifier: "T-5", PlanKey: "billing/b1", Kind: domain.IssueKindStory, State: "Backlog", BlockedBy: []string{"a3", "a4", "legal"}},
		{ID: "retry", Identifier: "T-6", PlanKey: "billing/retry", Kind: domain.IssueKindStory, State: "Done", BlockedBy: []string{}},
		{ID: "legal", Identifier: "T-88", Title: "Legal approves wording", State: "In Progress", BlockedBy: []string{}},
	}}
}

func nodeByKey(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	for _, n := range body["nodes"].([]any) {
		node := n.(map[string]any)
		if node["plan_key"] == key {
			return node
		}
	}
	t.Fatalf("no node %s in %v", key, body["nodes"])
	return nil
}

func TestPlanGraph_PlanControlFields(t *testing.T) {
	t.Parallel()
	rows := rowsLoader{
		"a3":    {{IssueID: "a3", Kind: persistence.PlanSatisfactionMerged, Ref: "abc123", PR: "acme/api#42"}},
		"retry": {{IssueID: "retry", Kind: persistence.PlanSatisfactionNoChange, Ref: "plan:billing/a3", Cites: "a3"}},
	}
	ts := planControlGraphServer(t, graphTracker(), rows, true)
	code, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/graph", "", planTestToken)
	if code != http.StatusOK {
		t.Fatalf("status %d %v", code, body)
	}
	if ps, _ := body["plan_states"].(map[string]any); ps["release"] != "Todo" || ps["backlog"] != "Backlog" || ps["cancel"] != "Canceled" || ps["done"] != "Done" {
		t.Errorf("plan_states = %v", body["plan_states"])
	}
	if len(body["nodes"].([]any)) != 4 {
		t.Errorf("nodes = %d, want the 4 plan-keyed issues only", len(body["nodes"].([]any)))
	}

	a3 := nodeByKey(t, body, "billing/a3")
	if a3["satisfaction"] != "merged" || a3["merge_commit"] != "abc123" || a3["pull_request"] != "acme/api#42" || a3["review"] != "human" {
		t.Errorf("a3 = %v, want merged with its commit, pull request, and review gate", a3)
	}
	if by, _ := a3["approved_by"].(map[string]any); by["name"] != "Ana" || a3["approved_at"] != "2026-10-16T14:02:00Z" {
		t.Errorf("a3 approver = %v %v", a3["approved_by"], a3["approved_at"])
	}

	b1 := nodeByKey(t, body, "billing/b1")
	reasons := map[string]string{}
	for _, w := range b1["waiting_on"].([]any) {
		m := w.(map[string]any)
		reasons[m["blocker_id"].(string)] = m["reason"].(string)
	}
	if len(reasons) != 2 || reasons["a4"] != "canceled" || reasons["legal"] != "not_done" {
		t.Errorf("b1 waits on %v, want a4 canceled and legal not_done (a3 merged)", reasons)
	}

	retry := nodeByKey(t, body, "billing/retry")
	if retry["satisfaction"] != "no_change_confirmed" || retry["proposed_supersede"] != "billing/a3" {
		t.Errorf("retry = %v, want a confirmed claim proposing supersede by billing/a3", retry)
	}
}

// With plan control off the fields are present and null.
func TestPlanGraph_PlanControlFieldsNullWhenOff(t *testing.T) {
	t.Parallel()
	ts := planControlGraphServer(t, graphTracker(), rowsLoader{}, false)
	_, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/graph", "", planTestToken)
	if v, present := body["plan_states"]; !present || v != nil {
		t.Errorf("plan_states = %v (present %v), want null", v, present)
	}
	b1 := nodeByKey(t, body, "billing/b1")
	for _, f := range []string{"review", "approved_by", "approved_at", "satisfaction", "unverified_reason", "merge_commit", "pull_request", "waiting_on", "proposed_supersede"} {
		v, present := b1[f]
		if !present || v != nil {
			t.Errorf("%s = %v (present %v), want present and null", f, v, present)
		}
	}
}

// A blocker another Sortie runs waits on its state alone: the graph holds
// none of its merge records, and it is satisfied once Done.
func TestPlanGraph_BlockerAnotherSortieRunsIsJudgedByItsState(t *testing.T) {
	t.Parallel()
	tr := &planTracker{issues: []domain.PlanIssue{
		{ID: "shape", Identifier: "T-16", PlanKey: "smolvm-machine-shape", Kind: domain.IssueKindStory, State: "Done",
			BlockedBy: []string{}, Labels: []string{"sortie-repository:sortie"}},
		{ID: "own", Identifier: "T-17", PlanKey: "homelab-own", Kind: domain.IssueKindStory, State: "Done",
			BlockedBy: []string{}, Labels: []string{"sortie-repository:homelab"}},
		{ID: "pilot", Identifier: "T-19", PlanKey: "homelab-pilot", Kind: domain.IssueKindStory, State: "Todo",
			BlockedBy: []string{"shape", "own"}, Labels: []string{"sortie-repository:homelab"}},
	}}
	states := domain.TrackerStates{Release: "Todo", Backlog: "Backlog", Cancel: "Canceled", Done: "Done"}
	srv := New(Params{
		SnapshotFn:   fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:    acceptingRefresh(),
		Logger:       slog.New(slog.DiscardHandler),
		ControlToken: planTestToken,
		PlanProjects: plan.NewProjects("T", plan.NewSyncer(plan.Tracker{Adapter: tr, Meta: registry.TrackerMeta{
			WriteCapabilities: registry.AllWriteCapabilities(), PlanKeyStorage: registry.PlanKeyCustomField,
		}}), nil),
		TerminalStatesFn: func() []string { return []string{"Done", "Canceled"} },
		PlanStatesFn:     func() domain.TrackerStates { return states },
		PlanSatisfaction: rowsLoader{},
		PlanRunsElsewhereFn: func() func(domain.BlockerRef) bool {
			return func(b domain.BlockerRef) bool { return slices.Contains(b.Labels, "sortie-repository:sortie") }
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	code, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/graph", "", planTestToken)
	if code != http.StatusOK {
		t.Fatalf("status %d %v", code, body)
	}
	reasons := map[string]string{}
	for _, w := range nodeByKey(t, body, "homelab-pilot")["waiting_on"].([]any) {
		m := w.(map[string]any)
		reasons[m["blocker_id"].(string)] = m["reason"].(string)
	}
	if len(reasons) != 1 || reasons["own"] != orchestrator.BlockerAwaitingMerge {
		t.Errorf("homelab-pilot waits on %v, want only its own blocker awaiting its merge record", reasons)
	}
}
