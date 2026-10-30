package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
