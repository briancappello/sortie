package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/plan"
	"github.com/sortie-ai/sortie/internal/registry"
)

const planTestToken = "control-secret"

// planTracker is an in-memory tracker with every write capability.
type planTracker struct {
	domain.TrackerAdapter

	mu      sync.Mutex
	issues  []domain.PlanIssue
	writes  int
	reads   int
	failAt  int
	listErr error
}

var (
	_ domain.PlanKeyFinder    = (*planTracker)(nil)
	_ domain.IssueCreator     = (*planTracker)(nil)
	_ domain.IssueUpdater     = (*planTracker)(nil)
	_ domain.IssueParenter    = (*planTracker)(nil)
	_ domain.DependencyLinker = (*planTracker)(nil)
)

func (p *planTracker) find(id string) *domain.PlanIssue {
	for i := range p.issues {
		if p.issues[i].ID == id {
			return &p.issues[i]
		}
	}
	return nil
}

func (p *planTracker) write() error {
	p.writes++
	if p.failAt > 0 && p.writes == p.failAt {
		return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "injected"}
	}
	return nil
}

func (p *planTracker) FindIssueByPlanKey(context.Context, string) (domain.PlanIssue, error) {
	return domain.PlanIssue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound}
}

func (p *planTracker) ListPlanIssues(_ context.Context, includeUnkeyed bool) ([]domain.PlanIssue, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.listErr != nil {
		return nil, p.listErr
	}
	out := []domain.PlanIssue{}
	for _, i := range p.issues {
		if includeUnkeyed || i.PlanKey != "" {
			i.BlockedBy = slices.Clone(i.BlockedBy)
			out = append(out, i)
		}
	}
	return out, nil
}

func (p *planTracker) CreateIssue(_ context.Context, in domain.NewIssue) (domain.CreatedIssue, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.write(); err != nil {
		return domain.CreatedIssue{}, err
	}
	id := fmt.Sprintf("id%d", len(p.issues)+1)
	p.issues = append(p.issues, domain.PlanIssue{
		ID: id, Identifier: "T-" + id, PlanKey: in.PlanKey, Kind: in.Kind, Title: in.Title,
		BodyDigest: plan.BodyDigest(in.Body), State: "Todo", URL: "https://t/" + id, ParentID: in.ParentID, BlockedBy: []string{},
	})
	return domain.CreatedIssue{ID: id, Identifier: "T-" + id}, nil
}

func (p *planTracker) UpdateIssue(context.Context, string, domain.IssueFields) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.write()
}

func (p *planTracker) SetParent(context.Context, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.write()
}

func (p *planTracker) AddDependency(_ context.Context, blocker, blocked string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.write(); err != nil {
		return err
	}
	b := p.find(blocked)
	b.BlockedBy = append(b.BlockedBy, blocker)
	return nil
}

func (p *planTracker) RemoveDependency(context.Context, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.write()
}

func planTestServer(t *testing.T, tr *planTracker, caps []registry.WriteCapability) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn:   fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:    acceptingRefresh(),
		Logger:       slog.New(slog.DiscardHandler),
		ControlToken: planTestToken,
		PlanProjects: plan.NewProjects("T", plan.NewSyncer(plan.Tracker{Adapter: tr, Meta: registry.TrackerMeta{
			WriteCapabilities: caps, PlanKeyStorage: registry.PlanKeyCustomField,
		}}), nil),
		TerminalStatesFn: func() []string { return []string{"Done"} },
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func planRequest(t *testing.T, ts *httptest.Server, method, path, body string, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s %s: body is not JSON: %s", method, path, raw)
	}
	return resp.StatusCode, decoded
}

// testGraph is the spec's first sync: accounts-api with three approved
// deliverables and billing with two.
func testGraph(version int, digest int) string {
	var nodes, parents []string
	add := func(key, kind, change string) {
		dk := "null"
		if kind == "story" {
			dk = `"story"`
		}
		nodes = append(nodes, fmt.Sprintf(`{"plan_key":%q,"kind":%q,"change":%q,"title":%q,"body":"b","approval":"approved","has_spec":true,"depth":0,"deliverable_kind":%s,"covers":[],"acceptance_criteria":[]}`,
			key, kind, change, key, dk))
	}
	for _, c := range []struct {
		name    string
		stories []string
	}{{"accounts-api", []string{"open", "close", "audit"}}, {"billing", []string{"invoice", "tax"}}} {
		add(c.name, "epic", c.name)
		for _, s := range c.stories {
			add(c.name+"/"+s, "story", c.name)
			parents = append(parents, fmt.Sprintf(`{"child":"%s/%s","parent":%q}`, c.name, s, c.name))
		}
	}
	return fmt.Sprintf(`{"schema_version":%d,"source_digest":"sha256:%064d","nodes":[%s],"parents":[%s],
		"blocks":[{"blocker":"accounts-api","blocked":"billing"},{"blocker":"accounts-api/close","blocked":"billing/invoice"}],"unplanned_changes":[]}`,
		version, digest, strings.Join(nodes, ","), strings.Join(parents, ","))
}

func syncBody(graph string, apply bool, previewID string) string {
	return fmt.Sprintf(`{"graph":%s,"apply":%t,"preview_id":%q}`, graph, apply, previewID)
}

func previewAndApply(t *testing.T, ts *httptest.Server, graph string) (int, map[string]any) {
	t.Helper()
	code, body := planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(graph, false, ""), planTestToken)
	if code != http.StatusOK {
		t.Fatalf("preview = %d %v", code, body)
	}
	return planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(graph, true, body["preview_id"].(string)), planTestToken)
}

func TestPlanSyncPreviewWritesNothingThenApplies(t *testing.T) {
	t.Parallel()

	tr := &planTracker{}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())

	code, body := planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), false, ""), planTestToken)
	if code != http.StatusOK || body["preview_id"] == "" {
		t.Fatalf("preview = %d %v", code, body)
	}
	counts := body["preview"].(map[string]any)["counts"].(map[string]any)
	if counts["creates"] != float64(7) || counts["relations_added"] != float64(2) || tr.writes != 0 {
		t.Errorf("preview counts %v, writes %d; want 7 creates, 2 relations, no writes", counts, tr.writes)
	}

	code, body = planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), true, body["preview_id"].(string)), planTestToken)
	if code != http.StatusOK || len(body["result"].(map[string]any)["created"].([]any)) != 7 {
		t.Fatalf("apply = %d %v", code, body)
	}

	// The second preview lists no writes.
	_, body = planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), false, ""), planTestToken)
	if c := body["preview"].(map[string]any)["counts"].(map[string]any); c["creates"] != float64(0) || c["relations_added"] != float64(0) {
		t.Errorf("second preview counts = %v, want none", c)
	}
}

func TestPlanSyncRejectsUnsupportedVersionWithoutTrackerRequest(t *testing.T) {
	t.Parallel()

	tr := &planTracker{}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())
	code, body := planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(9, 1), false, ""), planTestToken)
	msg := body["error"].(map[string]any)["message"].(string)
	if code != http.StatusBadRequest || body["error"].(map[string]any)["code"] != "invalid_graph" ||
		!strings.Contains(msg, "schema_version 9") || !strings.Contains(msg, "versions 1 to 2") {
		t.Errorf("response = %d %v", code, body)
	}
	if tr.reads != 0 || tr.writes != 0 {
		t.Errorf("tracker requests = %d reads, %d writes; want none", tr.reads, tr.writes)
	}
}

func TestPlanSyncStalePreviewIsAConflict(t *testing.T) {
	t.Parallel()

	tr := &planTracker{}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())
	_, body := planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), false, ""), planTestToken)
	code, body := planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 2), true, body["preview_id"].(string)), planTestToken)
	if code != http.StatusConflict || body["error"].(map[string]any)["code"] != "stale_preview" || tr.writes != 0 {
		t.Errorf("response = %d %v, writes %d", code, body, tr.writes)
	}
	code, _ = planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), true, ""), planTestToken)
	if code != http.StatusBadRequest {
		t.Errorf("apply without preview_id = %d, want 400", code)
	}
}

func TestPlanSyncMissingCapability(t *testing.T) {
	t.Parallel()

	tr := &planTracker{}
	ts := planTestServer(t, tr, []registry.WriteCapability{registry.CapCreateIssue, registry.CapSetParent})
	code, body := previewAndApply(t, ts, testGraph(1, 1))
	e := body["error"].(map[string]any)
	if code != http.StatusUnprocessableEntity || e["code"] != "tracker_unsupported_operation" ||
		!strings.Contains(e["message"].(string), "link_dependency") || tr.writes != 0 {
		t.Errorf("response = %d %v, writes %d", code, body, tr.writes)
	}
}

func TestPlanSyncPartialFailureReportsProgress(t *testing.T) {
	t.Parallel()

	tr := &planTracker{failAt: 4}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())
	code, body := previewAndApply(t, ts, testGraph(1, 1))
	result := body["result"].(map[string]any)
	if code != http.StatusBadGateway || body["error"].(map[string]any)["code"] != "tracker_transport_error" ||
		len(result["created"].([]any)) != 3 || result["failed"] == nil {
		t.Fatalf("response = %d %v", code, body)
	}
	tr.failAt = 0
	_, body = previewAndApply(t, ts, testGraph(1, 1))
	if n := len(body["result"].(map[string]any)["created"].([]any)); n != 4 {
		t.Errorf("resume created %d, want the remaining 4", n)
	}
}

func TestPlanEndpointsRequireTheControlToken(t *testing.T) {
	t.Parallel()

	tr := &planTracker{}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())
	for _, r := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), false, "")},
		{http.MethodGet, "/api/v1/plan/graph", ""},
		{http.MethodGet, "/api/v1/plan/capabilities", ""},
	} {
		if code, _ := planRequest(t, ts, r.method, r.path, r.body, ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", r.method, r.path, code)
		}
		if code, _ := planRequest(t, ts, r.method, r.path, r.body, "wrong"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token = %d, want 401", r.method, r.path, code)
		}
	}
	if tr.reads != 0 {
		t.Errorf("rejected requests reached the tracker %d times", tr.reads)
	}
	if code, _ := planRequest(t, ts, http.MethodPost, "/api/v1/plan/graph", "", planTestToken); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/plan/graph = %d, want 405", code)
	}
}

func TestPlanCapabilities(t *testing.T) {
	t.Parallel()

	ts := planTestServer(t, &planTracker{}, []registry.WriteCapability{registry.CapCreateIssue, registry.CapLinkDependency})
	code, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/capabilities", "", planTestToken)
	if code != http.StatusOK || fmt.Sprint(body["capabilities"]) != "[create_issue link_dependency]" || body["plan_key_storage"] != "custom_field" {
		t.Errorf("capabilities = %d %v", code, body)
	}
}

func TestPlanGraphAfterSyncWithRuntimeState(t *testing.T) {
	t.Parallel()

	tr := &planTracker{}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())
	if code, body := previewAndApply(t, ts, testGraph(1, 1)); code != http.StatusOK {
		t.Fatalf("sync = %d %v", code, body)
	}
	// A bug nobody planned, and a running worker on billing/invoice.
	tr.issues = append(tr.issues, domain.PlanIssue{ID: "bug", Identifier: "T-bug", Title: "Bug", State: "Done", BlockedBy: []string{}})
	var invoice domain.PlanIssue
	for _, i := range tr.issues {
		if i.PlanKey == "billing/invoice" {
			invoice = i
		}
	}
	snap := orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{
		{IssueID: invoice.ID, Identifier: invoice.Identifier, TurnCount: 3, LastAgentMessage: "writing tests"},
	}}
	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap), RefreshFn: acceptingRefresh(), Logger: slog.New(slog.DiscardHandler),
		ControlToken: planTestToken, TerminalStatesFn: func() []string { return []string{"done"} },
		PlanProjects: plan.NewProjects("T", plan.NewSyncer(plan.Tracker{Adapter: tr, Meta: registry.TrackerMeta{
			WriteCapabilities: registry.AllWriteCapabilities(), PlanKeyStorage: registry.PlanKeyCustomField,
		}}), nil),
	})
	ts2 := httptest.NewServer(srv.Mux())
	t.Cleanup(ts2.Close)

	code, body := planRequest(t, ts2, http.MethodGet, "/api/v1/plan/graph", "", planTestToken)
	nodes := body["nodes"].([]any)
	if code != http.StatusOK || len(nodes) != 7 || len(body["edges"].([]any)) != 2 || body["generated_at"] == nil {
		t.Fatalf("graph = %d: %d nodes, edges %v", code, len(nodes), body["edges"])
	}
	fields := []string{"id", "identifier", "plan_key", "kind", "title", "state", "terminal", "url", "parent_id", "runtime"}
	for _, raw := range nodes {
		n := raw.(map[string]any)
		for _, f := range fields {
			if _, ok := n[f]; !ok {
				t.Errorf("node %v lacks field %s", n["plan_key"], f)
			}
		}
		switch n["plan_key"] {
		case "billing/invoice":
			rt := n["runtime"].(map[string]any)
			if rt["status"] != "running" || rt["turn_count"] != float64(3) || rt["last_message"] != "writing tests" {
				t.Errorf("billing/invoice runtime = %v", rt)
			}
			if n["parent_id"] == nil || n["kind"] != "story" {
				t.Errorf("billing/invoice = %v, want a story with a parent", n)
			}
		case "billing":
			if n["runtime"] != nil || n["parent_id"] != nil {
				t.Errorf("billing = %v, want no runtime and no parent", n)
			}
		}
	}

	_, body = planRequest(t, ts2, http.MethodGet, "/api/v1/plan/graph?include_unkeyed=true", "", planTestToken)
	nodes = body["nodes"].([]any)
	bug := nodes[len(nodes)-1].(map[string]any)
	if len(nodes) != 8 || bug["plan_key"] != nil || bug["terminal"] != true || bug["kind"] != nil {
		t.Errorf("with unkeyed: %d nodes, bug %v", len(nodes), bug)
	}
}

func TestPlanGraphTrackerDownReturnsNoNodes(t *testing.T) {
	t.Parallel()

	tr := &planTracker{listErr: &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "connection refused"}}
	ts := planTestServer(t, tr, registry.AllWriteCapabilities())
	code, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/graph", "", planTestToken)
	if code != http.StatusBadGateway || body["error"].(map[string]any)["code"] != "tracker_transport_error" {
		t.Errorf("response = %d %v", code, body)
	}
	if _, ok := body["nodes"]; ok {
		t.Errorf("an error response carries nodes: %v", body)
	}
}

func TestPlanRoutesUnconfigured(t *testing.T) {
	t.Parallel()

	srv := New(Params{SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), RefreshFn: acceptingRefresh(), Logger: slog.New(slog.DiscardHandler)})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	if code, _ := planRequest(t, ts, http.MethodGet, "/api/v1/plan/capabilities", "", ""); code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured capabilities = %d, want 503", code)
	}
}
