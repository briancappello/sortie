package server

import (
	"fmt"
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

const (
	tokenLive1  = "session-token-for-live-1-aaaa"
	tokenLive2  = "session-token-for-live-2-bbbb"
	tokenEnded  = "session-token-of-an-ended-run"
	stubSuccess = http.StatusTeapot
)

// sessionScopedRoutes lists the routes a session token may call for its
// own issue, as a path format taking the issue identifier.
var sessionScopedRoutes = []string{"/api/v1/%s/stub-session-route"}

// registerSessionStubs mounts a stub handler behind the session scope
// for every route in sessionScopedRoutes that the server does not serve
// itself, and returns a counter of the requests that reached a handler.
func registerSessionStubs(h *authHarness) *atomic.Int64 {
	var reached atomic.Int64
	for _, route := range sessionScopedRoutes {
		if !strings.Contains(route, "stub-") {
			continue
		}
		pattern := "POST " + strings.Replace(route, "%s", "{identifier}", 1)
		h.srv.Mux().HandleFunc(pattern, h.srv.requireControl(scopeSession, func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.WriteHeader(stubSuccess)
		}))
	}
	return &reached
}

func TestSessionToken_Scope(t *testing.T) {
	t.Parallel()

	sessions := map[string]string{tokenLive1: "LIVE-1", tokenLive2: "LIVE-2"}
	for _, controlToken := range []string{"", testControlToken} {
		for _, route := range sessionScopedRoutes {
			tests := []struct {
				name       string
				issue      string
				token      string
				wantStatus int
			}{
				{"own issue", "LIVE-1", tokenLive1, stubSuccess},
				{"another issue", "LIVE-2", tokenLive1, http.StatusForbidden},
				{"unknown token", "LIVE-1", "not-a-session-token", http.StatusUnauthorized},
				{"token of an ended run", "LIVE-1", tokenEnded, http.StatusUnauthorized},
			}
			for _, tt := range tests {
				name := fmt.Sprintf("control token set %v/%s/%s", controlToken != "", route, tt.name)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					h := newAuthHarness(t, controlToken, sessions)
					reached := registerSessionStubs(h)
					status := h.do(t, http.MethodPost, fmt.Sprintf(route, tt.issue), "Bearer "+tt.token)
					if status != tt.wantStatus {
						t.Fatalf("POST %s status = %d, want %d", fmt.Sprintf(route, tt.issue), status, tt.wantStatus)
					}
					if tt.wantStatus != stubSuccess && reached.Load() != 0 {
						t.Errorf("a rejected request reached the handler")
					}
				})
			}
		}
	}
}

func TestSessionToken_RefusedOnRefresh(t *testing.T) {
	t.Parallel()

	for _, controlToken := range []string{"", testControlToken} {
		h := newAuthHarness(t, controlToken, map[string]string{tokenLive1: "LIVE-1"})
		if status := h.do(t, http.MethodPost, "/api/v1/refresh", "Bearer "+tokenLive1); status != http.StatusForbidden {
			t.Errorf("POST /api/v1/refresh with a session token (control token set %v) status = %d, want 403", controlToken != "", status)
		}
		if got := h.refreshes.Load(); got != 0 {
			t.Errorf("refresh signals = %d after a rejected request, want 0", got)
		}
	}
}

func TestSessionToken_ControlTokenAcceptedOnSessionRoutes(t *testing.T) {
	t.Parallel()

	h := newAuthHarness(t, testControlToken, nil)
	registerSessionStubs(h)
	for _, route := range sessionScopedRoutes {
		if status := h.do(t, http.MethodPost, fmt.Sprintf(route, "ANY-1"), "Bearer "+testControlToken); status == http.StatusUnauthorized || status == http.StatusForbidden {
			t.Errorf("POST %s with the control token status = %d, want it accepted", route, status)
		}
		if status := h.do(t, http.MethodPost, fmt.Sprintf(route, "ANY-1"), ""); status != http.StatusUnauthorized {
			t.Errorf("POST %s without a token status = %d, want 401", route, status)
		}
	}
}
