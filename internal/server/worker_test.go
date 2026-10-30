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
	req.Header.Set("X-Worker", "taichi")
	req.Header.Set(worker.ProtocolHeader, "1")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const checkInBody = `{"os":"linux","arch":"x86_64","kvm":true,"nested":true,"backends":["smolvm"],"run_slots":2,"running":1,"free":{"cpu":16,"mem_gib":52,"disk_gib":210}}`

// worker-api: Mounted under the remote kind.
func TestMountedUnderTheRemoteKind(t *testing.T) {
	t.Parallel()

	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), time.Now, nil)
	ts := workerTestServer(t, func() *worker.Matcher { return m })
	if got := workerPost(t, ts, "/api/v1/workers/checkin", checkInBody); got != http.StatusOK {
		t.Fatalf("check-in under remote = %d, want 200", got)
	}
}

// worker-api: Not mounted under smolvm; worker-dispatch: Other kinds
// unchanged. The server gets no matcher under any kind but remote.
func TestNotMountedUnderSmolvm(t *testing.T) {
	t.Parallel()

	ts := workerTestServer(t, func() *worker.Matcher { return nil })
	if got := workerPost(t, ts, "/api/v1/workers/checkin", checkInBody); got != http.StatusNotFound {
		t.Fatalf("check-in under another kind = %d, want 404", got)
	}
	if body := getJSONMap(t, ts.URL+"/api/v1/state"); body["workers"] != nil {
		t.Fatalf("state lists workers under another kind: %v", body["workers"])
	}
}

// worker-dispatch: A running remote job.
func TestARunningRemoteJob(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	m := worker.NewMatcher(worker.NewTTLLeaser(time.Minute), func() time.Time { return now }, nil)
	ts := workerTestServer(t, func() *worker.Matcher { return m })
	m.Submit(worker.Job{ID: "ONS-7", Attempt: 1})
	m.Submit(worker.Job{ID: "ONS-8", Attempt: 1, Requires: worker.Requires{Capabilities: []string{"gpu"}}})
	if got := workerPost(t, ts, "/api/v1/workers/checkin", checkInBody); got != http.StatusOK {
		t.Fatal(got)
	}
	if got := workerPost(t, ts, "/api/v1/workers/poll", ""); got != http.StatusOK {
		t.Fatal(got)
	}
	ws, _ := getJSONMap(t, ts.URL+"/api/v1/state")["workers"].(map[string]any)
	workers, _ := ws["workers"].([]any)
	if len(workers) != 1 {
		t.Fatalf("workers = %v", ws)
	}
	w := workers[0].(map[string]any)
	decl, _ := w["declaration"].(map[string]any)
	free, _ := decl["free"].(map[string]any)
	if w["id"] != "taichi" || w["last_check_in"] != "2026-10-29T12:00:00Z" || decl["os"] != "linux" ||
		decl["nested"] != true || decl["running"] != float64(1) || free["disk_gib"] != float64(210) {
		t.Fatalf("worker = %v", w)
	}
	leases, _ := ws["leases"].([]any)
	if len(leases) != 1 {
		t.Fatalf("leases = %v", ws["leases"])
	}
	l := leases[0].(map[string]any)
	if l["job"] != "ONS-7" || l["attempt"] != float64(1) || l["worker"] != "taichi" || l["expires"] != "2026-10-29T12:01:00Z" {
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
	req.Header.Set("X-Worker", "taichi")
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
	for f := range j.Flows {
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
	flows := map[string]string{}
	for _, f := range []string{"stdio", "egress", "mcp", "bundles", "usage"} {
		flows[f] = "https://edge.example/" + f
	}
	m.Submit(worker.Job{ID: "ONS-7", Attempt: 1, Flows: flows})
	if got := workerPost(t, ts, "/api/v1/workers/checkin", checkInBody); got != http.StatusOK {
		t.Fatal(got)
	}
	offer, ok, err := m.Poll("taichi")
	if !ok || err != nil || len(offer.Flows) != 5 {
		t.Fatalf("offer %+v ok=%v err=%v", offer, ok, err)
	}
	mcp := offer.Flows["mcp"]
	if mcp.URL != "https://edge.example/mcp" || mcp.Token == "" {
		t.Fatalf("flow %+v", mcp)
	}

	status, body := introspect(t, ts, testControlToken, mcp.Token)
	if status != http.StatusOK || body["job"] != "ONS-7" || body["attempt"] != float64(1) || body["flow"] != "mcp" {
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
	for name, f := range offer.Flows {
		if strings.Contains(string(state), f.Token) {
			t.Fatalf("state shows the %s token", name)
		}
	}

	if err := m.Report("taichi", "ONS-7", 1, worker.Outcome{Succeeded: true, Machine: worker.MachineStopped}); err != nil {
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
