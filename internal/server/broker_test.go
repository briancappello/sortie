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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

type brokerHarness struct {
	ts       *httptest.Server
	reported map[string]orchestrator.BrokerAddress
}

func newBrokerHarness(t *testing.T) *brokerHarness {
	t.Helper()
	h := &brokerHarness{reported: map[string]orchestrator.BrokerAddress{}}
	snapshot := orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{
		{IssueID: "i1", Identifier: "LIVE-1", BrokerURL: "http://127.0.0.1:41001", StartedAt: time.Now()},
		{IssueID: "i2", Identifier: "LIVE-2", StartedAt: time.Now()},
	}}
	srv := New(Params{
		SnapshotFn:      fixedSnapshot(snapshot),
		RefreshFn:       func() bool { return true },
		Logger:          slog.New(slog.DiscardHandler),
		MetricsRegistry: prometheus.NewRegistry(),
		ControlToken:    testControlToken,
		SessionTokenFn: func(token string) (string, bool) {
			switch token {
			case "tok-live-1":
				return "LIVE-1", true
			case "tok-live-2":
				return "LIVE-2", true
			}
			return "", false
		},
		BrokerReportFn: func(identifier string, b orchestrator.BrokerAddress) orchestrator.ControlReply {
			if identifier == "GONE-1" {
				return orchestrator.ControlReply{Outcome: orchestrator.ControlUnknownIssue}
			}
			h.reported[identifier] = b
			return orchestrator.ControlReply{Outcome: orchestrator.ControlDone}
		},
		BrokerReadFn: func(identifier string) orchestrator.ControlReply {
			if b, ok := h.reported[identifier]; ok {
				return orchestrator.ControlReply{Outcome: orchestrator.ControlDone, Broker: b}
			}
			return orchestrator.ControlReply{Outcome: orchestrator.ControlUnknownIssue}
		},
	})
	h.ts = httptest.NewServer(srv.Mux())
	t.Cleanup(h.ts.Close)
	return h
}

func (h *brokerHarness) do(t *testing.T, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const report = `{"http_url":"http://127.0.0.1:41001","viewer_token":"viewer-secret-123456"}`

func TestBrokerReport(t *testing.T) {
	t.Parallel()
	h := newBrokerHarness(t)
	if code, _ := h.do(t, http.MethodPost, "/api/v1/LIVE-1/broker", "tok-live-1", report); code != http.StatusOK {
		t.Errorf("own session token = %d, want 200", code)
	}
	if h.reported["LIVE-1"].ViewerToken != "viewer-secret-123456" {
		t.Errorf("reported = %+v", h.reported["LIVE-1"])
	}
	if code, _ := h.do(t, http.MethodPost, "/api/v1/LIVE-1/broker", "tok-live-2", report); code != http.StatusForbidden {
		t.Errorf("another issue's session token = %d, want 403", code)
	}
	if code, _ := h.do(t, http.MethodPost, "/api/v1/GONE-1/broker", testControlToken, report); code != http.StatusNotFound {
		t.Errorf("no running worker = %d, want 404", code)
	}
	if code, _ := h.do(t, http.MethodPost, "/api/v1/LIVE-1/broker", "tok-live-1", `{}`); code != http.StatusBadRequest {
		t.Errorf("empty report = %d, want 400", code)
	}
}

func TestBrokerRead(t *testing.T) {
	t.Parallel()
	h := newBrokerHarness(t)
	h.do(t, http.MethodPost, "/api/v1/LIVE-1/broker", "tok-live-1", report)

	code, body := h.do(t, http.MethodGet, "/api/v1/LIVE-1/broker", testControlToken, "")
	if code != http.StatusOK || !strings.Contains(body, "viewer-secret-123456") {
		t.Errorf("control token read = %d %s, want the token", code, body)
	}
	if code, _ := h.do(t, http.MethodGet, "/api/v1/LIVE-1/broker", "tok-live-1", ""); code != http.StatusForbidden {
		t.Errorf("session token read = %d, want 403", code)
	}
	if code, _ := h.do(t, http.MethodGet, "/api/v1/LIVE-2/broker", testControlToken, ""); code != http.StatusNotFound {
		t.Errorf("unreported broker = %d, want 404", code)
	}
}

// The state shows the address and never the viewer token.
func TestStateCarriesTheBrokerURLOnly(t *testing.T) {
	t.Parallel()
	h := newBrokerHarness(t)
	_, body := h.do(t, http.MethodGet, "/api/v1/state", "", "")
	var state struct {
		Running []struct {
			IssueIdentifier string          `json:"issue_identifier"`
			Broker          json.RawMessage `json:"broker"`
		} `json:"running"`
	}
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range state.Running {
		got[r.IssueIdentifier] = string(r.Broker)
	}
	if got["LIVE-1"] != `{"http_url":"http://127.0.0.1:41001"}` || got["LIVE-2"] != "null" {
		t.Errorf("broker fields = %v, want the URL for LIVE-1 and null for LIVE-2", got)
	}
	if strings.Contains(body, "viewer") {
		t.Error("state response mentions a viewer token")
	}
}
