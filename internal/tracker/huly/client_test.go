package huly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// fakeHulyServer serves the front config, the account RPC, and the
// transactor and collaborator routes on one host, the way the self-hosted
// nginx front lays them out.
func fakeHulyServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"ACCOUNTS_URL":     srv.URL + "/_accounts",
			"COLLABORATOR_URL": strings.Replace(srv.URL, "http://", "ws://", 1) + "/_collaborator",
		})
	})
	mux.HandleFunc("POST /_accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer account-token" {
			writeJSON(t, w, map[string]any{"error": map[string]any{"code": "platform:status:Unauthorized"}})
			return
		}
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Method != "selectWorkspace" {
			t.Errorf("account request = %+v (%v)", req, err)
		}
		writeJSON(t, w, map[string]any{"result": map[string]any{
			"endpoint":     strings.Replace(srv.URL, "http://", "ws://", 1) + "/_transactor",
			"token":        "ws-token",
			"workspace":    "ws-uuid",
			"workspaceUrl": "acme",
		}})
	})
	mux.HandleFunc("GET /_transactor/api/v1/account/ws-uuid", func(w http.ResponseWriter, r *http.Request) {
		requireToken(t, r)
		writeJSON(t, w, map[string]any{"uuid": "acct", "primarySocialId": "social-1"})
	})
	mux.HandleFunc("POST /_transactor/api/v1/find-all/ws-uuid", func(w http.ResponseWriter, r *http.Request) {
		requireToken(t, r)
		var req struct {
			Class string `json:"_class"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Class != classProject {
			writeJSON(t, w, map[string]any{"dataType": "TotalArray", "total": 0, "value": []any{}})
			return
		}
		writeJSON(t, w, map[string]any{"dataType": "TotalArray", "total": 1, "value": []any{
			map[string]any{"_id": "proj1", "identifier": "HULY"},
		}})
	})
	mux.HandleFunc("POST /_transactor/api/v1/tx/ws-uuid", func(w http.ResponseWriter, r *http.Request) {
		requireToken(t, r)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"Invalid tx"}`)
	})
	mux.HandleFunc("POST /_collaborator/rpc/{doc}", func(w http.ResponseWriter, r *http.Request) {
		requireToken(t, r)
		if got := r.PathValue("doc"); got != "ws-uuid|tracker:class:Issue|issue1|description" {
			t.Errorf("collaborator document = %q", got)
		}
		if !strings.Contains(r.URL.RawPath, "%7C") {
			t.Errorf("collaborator path %q is not encodeURIComponent-escaped", r.URL.RawPath)
		}
		writeJSON(t, w, map[string]any{"content": map[string]any{"description": `{"type":"doc"}`}})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

func requireToken(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer ws-token" {
		t.Errorf("%s %s Authorization = %q, want the workspace token", r.Method, r.URL.Path, got)
	}
}

func TestConnectAndRPC(t *testing.T) {
	t.Parallel()

	srv := fakeHulyServer(t)
	ctx := context.Background()

	conn, err := connect(ctx, srv.URL+"/", "account-token", "acme", "sortie/test")
	if err != nil {
		t.Fatalf("connect() error = %v", err)
	}
	if conn.socialID != "social-1" || conn.login.WorkspaceURL != "acme" || conn.frontBaseURL != srv.URL {
		t.Errorf("connection = %+v", conn)
	}

	docs, err := conn.rpc.FindAll(ctx, classProject, map[string]any{"identifier": "HULY"}, nil)
	if err != nil || len(docs) != 1 {
		t.Fatalf("FindAll() = %d docs, %v", len(docs), err)
	}

	content, err := conn.rpc.GetMarkup(ctx, classIssue, "issue1", "description", "blob1")
	if err != nil || content != `{"type":"doc"}` {
		t.Errorf("GetMarkup() = %q, %v", content, err)
	}

	if err := conn.rpc.Tx(ctx, map[string]any{}); trackerKind(err) != domain.ErrTrackerPayload {
		t.Errorf("Tx() error = %v, want payload error for HTTP 400", err)
	}
}

func TestConnectRejectsFallbackWorkspace(t *testing.T) {
	t.Parallel()

	// The fake account service answers every name with workspace "acme",
	// as Huly does for a name that matches no workspace.
	srv := fakeHulyServer(t)
	_, err := connect(context.Background(), srv.URL, "account-token", "other", "sortie/test")
	if trackerKind(err) != domain.ErrTrackerNotFound {
		t.Fatalf("connect() error = %v, want not found", err)
	}
}

func TestConnectRejectsBadToken(t *testing.T) {
	t.Parallel()

	srv := fakeHulyServer(t)
	_, err := connect(context.Background(), srv.URL, "wrong", "acme", "sortie/test")
	if trackerKind(err) != domain.ErrTrackerAuth {
		t.Fatalf("connect() error = %v, want auth error", err)
	}
}
