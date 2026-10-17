package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/plan"
	"github.com/sortie-ai/sortie/internal/registry"
)

// planWorkspace is a tracker workspace of several projects, each an
// in-memory planTracker. Its configured project T can create others.
type planWorkspace struct {
	mu       sync.Mutex
	projects map[string]*planTracker
}

// ensuringTracker is the configured project's tracker: a planTracker
// that inspects and creates the workspace's projects.
type ensuringTracker struct {
	*planTracker
	ws *planWorkspace
}

func (e *ensuringTracker) InspectPlanProject(_ context.Context, key string) (domain.PlanProject, error) {
	e.ws.mu.Lock()
	defer e.ws.mu.Unlock()
	if _, ok := e.ws.projects[key]; !ok {
		return domain.PlanProject{Key: key}, nil
	}
	return domain.PlanProject{Key: key, Name: key + " project", Exists: true}, nil
}

func (e *ensuringTracker) EnsurePlanProject(_ context.Context, key, name string) (domain.PlanProject, error) {
	e.ws.mu.Lock()
	defer e.ws.mu.Unlock()
	_, ok := e.ws.projects[key]
	if !ok {
		e.ws.projects[key] = &planTracker{}
	}
	return domain.PlanProject{Key: key, Name: name, Exists: true, Created: !ok, URL: "http://tracker.test/" + key}, nil
}

func trackerOf(tr domain.TrackerAdapter) plan.Tracker {
	return plan.Tracker{Adapter: tr, Meta: registry.TrackerMeta{
		WriteCapabilities: registry.AllWriteCapabilities(), PlanKeyStorage: registry.PlanKeyCustomField,
	}}
}

// multiProjectServer serves projects T (configured) and BILL, with snap
// as the orchestrator's runtime state.
func multiProjectServer(t *testing.T, snap orchestrator.RuntimeSnapshotResult) (*httptest.Server, *planWorkspace) {
	t.Helper()
	ws := &planWorkspace{projects: map[string]*planTracker{"T": {}, "BILL": {}}}
	def := &ensuringTracker{planTracker: ws.projects["T"], ws: ws}
	factory := func(_ context.Context, key string) (plan.Tracker, func(), error) {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		return trackerOf(ws.projects[key]), nil, nil
	}
	srv := New(Params{
		SnapshotFn: fixedSnapshot(snap), RefreshFn: acceptingRefresh(), Logger: slog.New(slog.DiscardHandler),
		ControlToken: planTestToken, TerminalStatesFn: func() []string { return []string{"Done"} },
		PlanProjects: plan.NewProjects("T", plan.NewSyncer(trackerOf(def)), factory),
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, ws
}

func TestPlanSyncWritesIntoTheNamedProjectOnly(t *testing.T) {
	t.Parallel()

	ts, ws := multiProjectServer(t, orchestrator.RuntimeSnapshotResult{})
	code, body := planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync?project=BILL", syncBody(testGraph(1, 1), false, ""), planTestToken)
	if code != http.StatusOK {
		t.Fatalf("preview = %d %v", code, body)
	}
	id := body["preview_id"].(string)
	code, body = planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync?project=BILL", syncBody(testGraph(1, 1), true, id), planTestToken)
	if code != http.StatusOK {
		t.Fatalf("apply = %d %v", code, body)
	}
	if len(ws.projects["BILL"].issues) != 7 || len(ws.projects["T"].issues) != 0 {
		t.Errorf("issues: BILL %d, T %d; want 7 in BILL only", len(ws.projects["BILL"].issues), len(ws.projects["T"].issues))
	}

	// A preview of one project cannot be applied to another.
	code, body = planRequest(t, ts, http.MethodPost, "/api/v1/plan/sync", syncBody(testGraph(1, 1), true, id), planTestToken)
	if code != http.StatusConflict || errorCode(body) != "stale_preview" {
		t.Errorf("apply of BILL's preview to T = %d %v, want 409 stale_preview", code, body)
	}
}

func TestPlanGraphCarriesRuntimeForTheDispatchedProjectOnly(t *testing.T) {
	t.Parallel()

	shared := domain.PlanIssue{ID: "i1", Identifier: "X-1", Title: "One", State: "Todo", PlanKey: "a", Kind: domain.IssueKindEpic, BlockedBy: []string{}}
	snap := orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{{IssueID: "i1", Identifier: "X-1", TurnCount: 2}}}
	ts, workspace := multiProjectServer(t, snap)
	workspace.projects["T"].issues = []domain.PlanIssue{shared}
	workspace.projects["BILL"].issues = []domain.PlanIssue{shared}

	for _, tc := range []struct {
		path       string
		project    string
		dispatched bool
	}{
		{"/api/v1/plan/graph", "T", true},
		{"/api/v1/plan/graph?project=T", "T", true},
		{"/api/v1/plan/graph?project=BILL", "BILL", false},
	} {
		code, body := planRequest(t, ts, http.MethodGet, tc.path, "", planTestToken)
		if code != http.StatusOK || body["project"] != tc.project || body["dispatched"] != tc.dispatched {
			t.Errorf("%s = %d project %v dispatched %v, want %s %v", tc.path, code, body["project"], body["dispatched"], tc.project, tc.dispatched)
			continue
		}
		node := body["nodes"].([]any)[0].(map[string]any)
		if hasRuntime := node["runtime"] != nil; hasRuntime != tc.dispatched {
			t.Errorf("%s: runtime = %v, want present %v", tc.path, node["runtime"], tc.dispatched)
		}
	}
}

func TestPlanRoutesRefuseAProjectTheWorkspaceLacks(t *testing.T) {
	t.Parallel()

	ts, _ := multiProjectServer(t, orchestrator.RuntimeSnapshotResult{})
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/plan/graph?project=NOPE", ""},
		{http.MethodGet, "/api/v1/plan/capabilities?project=NOPE", ""},
		{http.MethodPost, "/api/v1/plan/sync?project=NOPE", syncBody(testGraph(1, 1), false, "")},
	} {
		code, body := planRequest(t, ts, req.method, req.path, req.body, planTestToken)
		if code != http.StatusNotFound || errorCode(body) != "project_not_found" {
			t.Errorf("%s %s = %d %v, want 404 project_not_found", req.method, req.path, code, body)
		}
	}
}

func TestPlanProjectInspectThenEnsure(t *testing.T) {
	t.Parallel()

	ts, ws := multiProjectServer(t, orchestrator.RuntimeSnapshotResult{})

	code, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/projects/NEW", "", planTestToken)
	if code != http.StatusOK || body["exists"] != false || body["ready"] != false || body["name"] != nil {
		t.Errorf("inspect NEW = %d %v, want it missing", code, body)
	}

	code, body = planRequest(t, ts, http.MethodPut, "/api/v1/plan/projects/NEW", `{"name":"New work"}`, planTestToken)
	if code != http.StatusOK || body["created"] != true || body["ready"] != true || body["name"] != "New work" ||
		body["dispatched"] != false || body["url"] != "http://tracker.test/NEW" {
		t.Errorf("ensure NEW = %d %v, want it created and ready", code, body)
	}
	if _, ok := ws.projects["NEW"]; !ok {
		t.Fatal("the workspace has no project NEW after ensure")
	}

	code, body = planRequest(t, ts, http.MethodPut, "/api/v1/plan/projects/NEW", `{"name":"New work"}`, planTestToken)
	if code != http.StatusOK || body["created"] != false {
		t.Errorf("second ensure = %d %v, want created false", code, body)
	}
	if code, body := planRequest(t, ts, http.MethodGet, "/api/v1/plan/graph?project=NEW", "", planTestToken); code != http.StatusOK {
		t.Errorf("graph of NEW = %d %v", code, body)
	}

	code, body = planRequest(t, ts, http.MethodGet, "/api/v1/plan/projects/T", "", planTestToken)
	if code != http.StatusOK || body["dispatched"] != true {
		t.Errorf("inspect T = %d %v, want dispatched", code, body)
	}
}

func TestPlanProjectRouteGuards(t *testing.T) {
	t.Parallel()

	ts, _ := multiProjectServer(t, orchestrator.RuntimeSnapshotResult{})
	if code, _ := planRequest(t, ts, http.MethodPut, "/api/v1/plan/projects/NEW", `{}`, ""); code != http.StatusUnauthorized {
		t.Errorf("ensure without token = %d, want 401", code)
	}
	if code, _ := planRequest(t, ts, http.MethodPost, "/api/v1/plan/projects/NEW", `{}`, planTestToken); code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", code)
	}
	if code, body := planRequest(t, ts, http.MethodPut, "/api/v1/plan/projects/NEW", `{"name":`, planTestToken); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d %v, want 400", code, body)
	}
}

func TestPlanProjectsUnsupportedByTheTracker(t *testing.T) {
	t.Parallel()

	ts := planTestServer(t, &planTracker{}, registry.AllWriteCapabilities())
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/plan/graph?project=OTHER"},
		{http.MethodGet, "/api/v1/plan/projects/OTHER"},
		{http.MethodPut, "/api/v1/plan/projects/OTHER"},
	} {
		code, body := planRequest(t, ts, req.method, req.path, "", planTestToken)
		if code != http.StatusUnprocessableEntity || errorCode(body) != "projects_unsupported" {
			t.Errorf("%s %s = %d %v, want 422 projects_unsupported", req.method, req.path, code, body)
		}
	}
	if code, _ := planRequest(t, ts, http.MethodGet, "/api/v1/plan/graph?project=T", "", planTestToken); code != http.StatusOK {
		t.Errorf("graph of the configured project by name = %d, want 200", code)
	}
}

func TestPlanProjectRouteUnconfigured(t *testing.T) {
	t.Parallel()

	srv := New(Params{SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), RefreshFn: acceptingRefresh(), Logger: slog.New(slog.DiscardHandler)})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	if code, _ := planRequest(t, ts, http.MethodGet, "/api/v1/plan/projects/T", "", ""); code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured project route = %d, want 503", code)
	}
}

func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}
