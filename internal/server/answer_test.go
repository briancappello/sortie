package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/parkanswer"
)

type recordedAnswer struct {
	identifier string
	answer     parkanswer.Answer
}

func answerTestServer(t *testing.T, controlToken string, sessions map[string]string, reply orchestrator.ControlReply, calls *[]recordedAnswer) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn:   fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:    acceptingRefresh(),
		Logger:       slog.New(slog.DiscardHandler),
		ControlToken: controlToken,
		SessionTokenFn: func(token string) (string, bool) {
			identifier, ok := sessions[token]
			return identifier, ok
		},
		AnswerFn: func(identifier string, answer parkanswer.Answer) orchestrator.ControlReply {
			*calls = append(*calls, recordedAnswer{identifier, answer})
			return reply
		},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func postAnswer(t *testing.T, ts *httptest.Server, path, authorization, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestAnswer_StatusCodes(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	answered := orchestrator.ControlReply{Outcome: orchestrator.ControlDone, Answer: orchestrator.ParkAnswer{
		IssueID: "id-25", Identifier: "ONS-25", Author: "Serghei", Reason: "agent_blocked",
		Statement: "Which repository should the daemon live in?", ParkedAt: parkedAt, ReleasedAt: parkedAt.Add(time.Hour),
	}}
	const body = `{"text":"agentic-orchestration","author":"Serghei"}`

	tests := []struct {
		name       string
		body       string
		reply      orchestrator.ControlReply
		wantStatus int
		wantCode   string
		wantCalls  int
	}{
		{"parked issue", body, answered, http.StatusOK, "", 1},
		{"issue that is not parked", body, orchestrator.ControlReply{Outcome: orchestrator.ControlNotParked, Message: `issue "ONS-25" is not parked`}, http.StatusConflict, "issue_not_parked", 1},
		{"empty text", `{"text":" ","author":"Serghei"}`, orchestrator.ControlReply{Outcome: orchestrator.ControlInvalid, Message: "text and author must not be empty"}, http.StatusBadRequest, "invalid_request", 1},
		{"comment write fails", body, orchestrator.ControlReply{Outcome: orchestrator.ControlTrackerFailed, Message: "tracker down"}, http.StatusBadGateway, "tracker_error", 1},
		{"orchestrator unavailable", body, orchestrator.ControlReply{Outcome: orchestrator.ControlUnavailable}, http.StatusServiceUnavailable, "orchestrator_unavailable", 1},
		{"malformed body", `{"text":`, answered, http.StatusBadRequest, "invalid_request", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls []recordedAnswer
			ts := answerTestServer(t, "", nil, tt.reply, &calls)

			status, resp := postControl(t, ts, http.MethodPost, "/api/v1/ONS-25/answer", tt.body)
			if status != tt.wantStatus {
				t.Fatalf("POST /api/v1/ONS-25/answer status = %d, want %d (body %v)", status, tt.wantStatus, resp)
			}
			if len(calls) != tt.wantCalls {
				t.Fatalf("AnswerFn calls = %d, want %d", len(calls), tt.wantCalls)
			}
			if tt.wantCode != "" {
				if errObj, _ := resp["error"].(map[string]any); errObj["code"] != tt.wantCode {
					t.Errorf("error body = %v, want code %q", resp, tt.wantCode)
				}
				return
			}
			if want := (recordedAnswer{"ONS-25", parkanswer.Answer{Text: "agentic-orchestration", Author: "Serghei"}}); calls[0] != want {
				t.Errorf("AnswerFn call = %+v, want %+v", calls[0], want)
			}
			for key, want := range map[string]any{
				"issue_identifier": "ONS-25", "issue_id": "id-25", "author": "Serghei", "reason": "agent_blocked",
				"statement": "Which repository should the daemon live in?", "parked_at": "2026-10-31T09:00:00Z",
				"released_at": "2026-10-31T10:00:00Z", "gesture": "answered",
			} {
				if resp[key] != want {
					t.Errorf("response %s = %v, want %v", key, resp[key], want)
				}
			}
		})
	}
}

func TestAnswer_ControlTokenOnly(t *testing.T) {
	t.Parallel()

	sessions := map[string]string{tokenLive1: "ONS-25"}
	tests := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{"control token", "Bearer " + testControlToken, http.StatusOK},
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"the issue's own session token", "Bearer " + tokenLive1, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls []recordedAnswer
			ts := answerTestServer(t, testControlToken, sessions, orchestrator.ControlReply{Outcome: orchestrator.ControlDone}, &calls)
			status := postAnswer(t, ts, "/api/v1/ONS-25/answer", tt.authorization, `{"text":"agentic-orchestration","author":"Serghei"}`)
			if status != tt.wantStatus {
				t.Fatalf("POST /api/v1/ONS-25/answer status = %d, want %d", status, tt.wantStatus)
			}
			if wantCalls := map[bool]int{true: 1, false: 0}[tt.wantStatus == http.StatusOK]; len(calls) != wantCalls {
				t.Errorf("AnswerFn calls = %d, want %d", len(calls), wantCalls)
			}
		})
	}
}

func TestAnswer_OnlyPost(t *testing.T) {
	t.Parallel()

	var calls []recordedAnswer
	ts := answerTestServer(t, "", nil, orchestrator.ControlReply{Outcome: orchestrator.ControlDone}, &calls)
	status, body := postControl(t, ts, http.MethodGet, "/api/v1/ONS-25/answer", "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/v1/ONS-25/answer status = %d, want 405 (body %v)", status, body)
	}
	if len(calls) != 0 {
		t.Errorf("AnswerFn calls = %d, want 0", len(calls))
	}
}

func TestAnswer_Unavailable(t *testing.T) {
	t.Parallel()

	ts := testServer(t, fixedSnapshot(orchestrator.RuntimeSnapshotResult{}), acceptingRefresh())
	if status, body := postControl(t, ts, http.MethodPost, "/api/v1/ONS-25/answer", `{"text":"a","author":"b"}`); status != http.StatusServiceUnavailable {
		t.Errorf("POST answer without AnswerFn status = %d, want 503 (body %v)", status, body)
	}
}

func TestDetail_ParkedCarriesStatementInFull(t *testing.T) {
	t.Parallel()

	statement := "Which repository should the daemon live in? " + strings.Repeat("It could be sortie or agentic-orchestration. ", 40)
	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC),
		Parked: []orchestrator.SnapshotParkedEntry{{
			IssueID: "id-25", Identifier: "ONS-25", Reason: "agent_blocked", Label: "needs-human",
			ParkedAt: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC), Statement: statement,
		}},
	}
	ts := testServer(t, fixedSnapshot(snap), acceptingRefresh())

	_, detail := postControl(t, ts, http.MethodGet, "/api/v1/ONS-25", "")
	if detail["status"] != "parked" {
		t.Fatalf("detail status = %v, want parked", detail["status"])
	}
	if parked, _ := detail["parked"].(map[string]any); parked["statement"] != statement {
		t.Errorf("detail parked.statement = %v, want the stop statement in full (%d bytes)", parked["statement"], len(statement))
	}
	_, state := postControl(t, ts, http.MethodGet, "/api/v1/state", "")
	if rows, _ := state["parked"].([]any); len(rows) != 1 || rows[0].(map[string]any)["statement"] != statement {
		t.Errorf("state parked = %v, want one row carrying the statement", state["parked"])
	}
}
