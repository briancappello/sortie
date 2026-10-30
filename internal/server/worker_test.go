package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/jobtoken"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/worker"
)

func workerTestServer(t *testing.T, matcher func() *worker.Matcher) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn:             fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:              acceptingRefresh(),
		Logger:                 slog.New(slog.DiscardHandler),
		WorkersFn:              matcher,
		WorkerIdentityHeaderFn: func() string { return "X-Worker" },
	})
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func workerPost(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Worker", "w1")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const checkInBody = `{"protocol":1,"capabilities":["linux"],"resources":{"cpus":4,"memory_mib":4096,"disk_mib":1000,"run_slots":1}}`

// The worker API is served under the remote kind only.
func TestWorkerAPIServedUnderRemoteKindOnly(t *testing.T) {
	t.Parallel()

	var m *worker.Matcher
	ts := workerTestServer(t, func() *worker.Matcher { return m })
	if got := workerPost(t, ts, "/api/v1/worker/checkin", checkInBody); got != http.StatusNotFound {
		t.Fatalf("check-in under another kind = %d, want 404", got)
	}
	if body := getJSONMap(t, ts.URL+"/api/v1/state"); body["workers"] != nil {
		t.Fatalf("state lists workers under another kind: %v", body["workers"])
	}
	m = worker.NewMatcher(worker.NewTTLLeaser(time.Minute), time.Now, nil)
	if got := workerPost(t, ts, "/api/v1/worker/checkin", checkInBody); got != http.StatusOK {
		t.Fatalf("check-in under remote = %d, want 200", got)
	}
}

// Workers and leases are visible in state.
func TestStateListsWorkersAndLeases(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), func() time.Time { return now }, nil)
	ts := workerTestServer(t, func() *worker.Matcher { return m })
	m.Submit(worker.Job{ID: "D1", Identifier: "ONS-7", Attempt: 2})
	m.Submit(worker.Job{ID: "D2", Identifier: "ONS-8", Requires: []string{"gpu"}})
	if got := workerPost(t, ts, "/api/v1/worker/checkin", checkInBody); got != http.StatusOK {
		t.Fatal(got)
	}
	if got := workerPost(t, ts, "/api/v1/worker/poll", ""); got != http.StatusOK {
		t.Fatal(got)
	}
	ws, _ := getJSONMap(t, ts.URL+"/api/v1/state")["workers"].(map[string]any)
	workers, _ := ws["workers"].([]any)
	if len(workers) != 1 {
		t.Fatalf("workers = %v", ws)
	}
	w := workers[0].(map[string]any)
	decl, _ := w["declaration"].(map[string]any)
	if w["id"] != "w1" || w["last_check_in"] != "2026-10-29T12:00:00Z" || decl["capabilities"] == nil || decl["resources"] == nil {
		t.Fatalf("worker = %v", w)
	}
	leases, _ := ws["leases"].([]any)
	if len(leases) != 1 {
		t.Fatalf("leases = %v", ws["leases"])
	}
	l := leases[0].(map[string]any)
	if l["job_id"] != "D1" || l["identifier"] != "ONS-7" || l["attempt"] != float64(2) || l["worker_id"] != "w1" || l["expires"] != "2026-10-29T12:01:00Z" {
		t.Fatalf("lease = %v", l)
	}
	queue, _ := ws["queue"].([]any)
	if len(queue) != 1 || !strings.Contains(queue[0].(map[string]any)["waiting"].([]any)[0].(string), "gpu") {
		t.Fatalf("queue = %v", ws["queue"])
	}
}

func introspectServer(t *testing.T, m *worker.Matcher, issuer func() *jobtoken.Issuer) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn:             fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:              acceptingRefresh(),
		Logger:                 slog.New(slog.DiscardHandler),
		WorkersFn:              func() *worker.Matcher { return m },
		WorkerIdentityHeaderFn: func() string { return "X-Worker" },
		JobTokensFn:            issuer,
		ControlToken:           testControlToken,
	})
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func introspect(t *testing.T, ts *httptest.Server, auth, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/tokens/introspect",
		strings.NewReader(`{"token":"`+token+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Worker", "w1")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

type issuerTokens struct{ i *jobtoken.Issuer }

func (x issuerTokens) Mint(j worker.Job) (map[string]string, error) {
	flows := make([]jobtoken.Flow, 0, len(j.Flows))
	for _, f := range j.Flows {
		flows = append(flows, jobtoken.Flow(f))
	}
	toks, err := x.i.Mint(j.ID, uint64(j.Attempt), flows)
	out := map[string]string{}
	for f, tok := range toks {
		out[string(f)] = tok.Reveal()
	}
	return out, err
}

func (x issuerTokens) Void(jobID string) { x.i.OnLeaseEnd(jobID) }

// A proxy holding the control token learns what a live job token
// grants; a void token is unknown and a worker cannot ask; state never
// shows a token.
func TestIntrospectJobTokens(t *testing.T) {
	t.Parallel()

	issuer := jobtoken.NewIssuer()
	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), time.Now, issuerTokens{issuer})
	ts := introspectServer(t, m, func() *jobtoken.Issuer { return issuer })
	m.Submit(worker.Job{ID: "D1", Identifier: "ONS-7", Attempt: 1, Flows: []string{"stdio", "egress", "mcp", "bundles", "usage"}})
	if got := workerPost(t, ts, "/api/v1/worker/checkin", checkInBody); got != http.StatusOK {
		t.Fatal(got)
	}
	offer, ok, err := m.Poll("w1")
	if !ok || err != nil || len(offer.Flows) != 5 {
		t.Fatalf("offer %+v ok=%v err=%v", offer, ok, err)
	}
	mcp := offer.Flows[2]
	if mcp.Name != "mcp" || mcp.Token == "" {
		t.Fatalf("flow %+v", mcp)
	}

	status, body := introspect(t, ts, testControlToken, mcp.Token)
	if status != http.StatusOK || body["job"] != "D1" || body["attempt"] != float64(1) || body["flow"] != "mcp" {
		t.Fatalf("introspect = %d %v", status, body)
	}
	if status, _ := introspect(t, ts, "", mcp.Token); status != http.StatusUnauthorized {
		t.Fatalf("introspect without the control token = %d, want 401", status)
	}
	if status, _ := introspect(t, ts, "worker-guess", mcp.Token); status != http.StatusUnauthorized {
		t.Fatalf("introspect with another bearer = %d, want 401", status)
	}

	stateReq, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/state", nil)
	resp, err := ts.Client().Do(stateReq)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(state), `"leases"`) {
		t.Fatalf("state lists no lease: %s", state)
	}
	for _, f := range offer.Flows {
		if strings.Contains(string(state), f.Token) {
			t.Fatalf("state shows the %s token", f.Name)
		}
	}

	if err := m.Report("w1", "D1", 1, worker.Outcome{Succeeded: true}); err != nil {
		t.Fatal(err)
	}
	if status, _ := introspect(t, ts, testControlToken, mcp.Token); status != http.StatusNotFound {
		t.Fatalf("introspect of a void token = %d, want 404", status)
	}
}

// Without job tokens, as under any kind but remote, every token is
// unknown.
func TestIntrospectWithoutJobTokens(t *testing.T) {
	t.Parallel()

	ts := introspectServer(t, nil, func() *jobtoken.Issuer { return nil })
	if status, _ := introspect(t, ts, testControlToken, "anything"); status != http.StatusNotFound {
		t.Fatalf("introspect = %d, want 404", status)
	}
}
