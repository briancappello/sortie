package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

const testControlToken = "operator-control-token-1234"

type authHarness struct {
	ts        *httptest.Server
	srv       *Server
	refreshes atomic.Int64
}

func newAuthHarness(t *testing.T, controlToken string, sessions map[string]string) *authHarness {
	t.Helper()
	h := &authHarness{}
	h.srv = New(Params{
		SnapshotFn:      fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:       func() bool { h.refreshes.Add(1); return true },
		Logger:          slog.New(slog.DiscardHandler),
		MetricsRegistry: prometheus.NewRegistry(),
		ControlToken:    controlToken,
		SessionTokenFn: func(token string) (string, bool) {
			identifier, ok := sessions[token]
			return identifier, ok
		},
	})
	h.ts = httptest.NewServer(h.srv.Mux())
	t.Cleanup(h.ts.Close)
	return h
}

func (h *authHarness) do(t *testing.T, method, path, authorization string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, h.ts.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestRefresh_ControlToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		controlToken  string
		authorization string
		wantStatus    int
	}{
		{"no token configured, no header", "", "", http.StatusAccepted},
		{"token configured, no header", testControlToken, "", http.StatusUnauthorized},
		{"token configured, wrong token", testControlToken, "Bearer wrong-token", http.StatusUnauthorized},
		{"token configured, not a bearer header", testControlToken, "Basic " + testControlToken, http.StatusUnauthorized},
		{"token configured, right token", testControlToken, "Bearer " + testControlToken, http.StatusAccepted},
		{"token configured, scheme in another case", testControlToken, "bearer " + testControlToken, http.StatusAccepted},
		{"no token configured, unknown token presented", "", "Bearer unknown", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newAuthHarness(t, tt.controlToken, nil)
			if status := h.do(t, http.MethodPost, "/api/v1/refresh", tt.authorization); status != tt.wantStatus {
				t.Fatalf("POST /api/v1/refresh status = %d, want %d", status, tt.wantStatus)
			}
			wantRefreshes := int64(0)
			if tt.wantStatus == http.StatusAccepted {
				wantRefreshes = 1
			}
			if got := h.refreshes.Load(); got != wantRefreshes {
				t.Errorf("refresh signals = %d, want %d", got, wantRefreshes)
			}
		})
	}
}

func TestReadOnlyRoutes_NeedNoToken(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, testControlToken, nil)
	for _, path := range []string{"/api/v1/state", "/metrics", "/livez", "/"} {
		if status := h.do(t, http.MethodGet, path, ""); status == http.StatusUnauthorized || status == http.StatusForbidden {
			t.Errorf("GET %s status = %d with a control token configured, want it served without one", path, status)
		}
	}
}
