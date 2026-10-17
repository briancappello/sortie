package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

func planControlServer(t *testing.T, outcome orchestrator.ControlOutcome, calls *int) *httptest.Server {
	t.Helper()
	fn := func(_ context.Context, key string) orchestrator.ControlReply {
		*calls++
		return orchestrator.ControlReply{Outcome: outcome, Message: "for " + key}
	}
	srv := New(Params{
		SnapshotFn:      fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:       func() bool { return true },
		Logger:          slog.New(slog.DiscardHandler),
		MetricsRegistry: prometheus.NewRegistry(),
		ControlToken:    testControlToken,
		SessionTokenFn: func(token string) (string, bool) {
			return "LIVE-1", token == "session-token"
		},
		PullBackFn:   fn,
		PlanCancelFn: fn,
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func postPlanControl(t *testing.T, ts *httptest.Server, path, auth, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestPlanControlRoutes_StatusCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		outcome  orchestrator.ControlOutcome
		status   int
		wantCode string
	}{
		{orchestrator.ControlDone, http.StatusOK, ""},
		{orchestrator.ControlUnknownIssue, http.StatusNotFound, "issue_not_found"},
		{orchestrator.ControlAlreadyClaimed, http.StatusConflict, "already_claimed"},
		{orchestrator.ControlNotReleased, http.StatusConflict, "not_released"},
		{orchestrator.ControlAlreadyTerminal, http.StatusConflict, "already_terminal"},
		{orchestrator.ControlPlanOff, http.StatusConflict, "plan_control_off"},
		{orchestrator.ControlTrackerFailed, http.StatusBadGateway, "tracker_error"},
	}
	for _, path := range []string{"/api/v1/plan/pull-back", "/api/v1/plan/cancel"} {
		for _, tt := range tests {
			t.Run(path+"/"+tt.wantCode, func(t *testing.T) {
				t.Parallel()
				calls := 0
				ts := planControlServer(t, tt.outcome, &calls)
				status, body := postPlanControl(t, ts, path, testControlToken, `{"plan_key":"billing/invoice"}`)
				if status != tt.status {
					t.Errorf("status = %d, want %d", status, tt.status)
				}
				if tt.wantCode != "" {
					errObj, _ := body["error"].(map[string]any)
					if errObj["code"] != tt.wantCode {
						t.Errorf("error = %v, want code %s", body, tt.wantCode)
					}
				}
			})
		}
	}
}

func TestPlanControlRoutes_Auth(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/v1/plan/pull-back", "/api/v1/plan/cancel"} {
		calls := 0
		ts := planControlServer(t, orchestrator.ControlDone, &calls)
		if status, _ := postPlanControl(t, ts, path, "", `{"plan_key":"k"}`); status != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, status)
		}
		if status, _ := postPlanControl(t, ts, path, "session-token", `{"plan_key":"k"}`); status != http.StatusForbidden {
			t.Errorf("%s with a session token = %d, want 403", path, status)
		}
		if status, _ := postPlanControl(t, ts, path, testControlToken, `{}`); status != http.StatusBadRequest {
			t.Errorf("%s without a plan key = %d, want 400", path, status)
		}
		if calls != 0 {
			t.Errorf("%s: control called %d times by refused requests", path, calls)
		}
	}
}
