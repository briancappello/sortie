package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/redact"
)

func envMap(t *testing.T, vars []domain.EnvVar) map[string]string {
	t.Helper()
	m := make(map[string]string, len(vars))
	for _, v := range vars {
		if _, dup := m[v.Name]; dup {
			t.Errorf("session env names %s twice", v.Name)
		}
		m[v.Name] = v.Value
	}
	return m
}

func (h *runHarness) waitForRunHistory(issueID string) persistence.RunHistory {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.store.mu.Lock()
		for _, row := range h.store.runHistories {
			if row.IssueID == issueID {
				h.store.mu.Unlock()
				return row
			}
		}
		h.store.mu.Unlock()
		if time.Now().After(deadline) {
			h.t.Fatalf("no run history row for %s after 5s; log: %s", issueID, h.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRun_SessionEnvMatchesRunHistory runs whole dispatches through the
// event loop and asserts that every session identity variable the agent
// launches receive equals the value recorded in the run's history row.
func TestRun_SessionEnvMatchesRunHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		issueID  string
		seed     func(h *runHarness)
		wantRule string
	}{
		{
			name:    "fresh dispatch on a named rule",
			issueID: "id-1",
			seed: func(h *runHarness) {
				h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1", "stage-implement")}
				h.build()
			},
			wantRule: "implement",
		},
		{
			name:    "retry carries its attempt",
			issueID: "id-retry",
			seed: func(h *runHarness) {
				h.tracker.issues["id-retry"] = candidate("id-retry", "R-1", "Stage-Plan")
				seedRetry(h, "scheduled", RetryEntry{Attempt: 2, AgentKind: "kind-b", RuleName: "plan"})
			},
			wantRule: "plan",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
			h.apiURL = "http://127.0.0.1:7678"
			tt.seed(h)
			h.run()
			row := h.waitForRunHistory(tt.issueID)
			h.stop()

			var work, check []domain.StartSessionParams
			for _, r := range h.recorders {
				work = append(work, r.started()...)
				check = append(check, r.verified()...)
			}
			if len(work) == 0 || len(check) == 0 {
				t.Fatalf("work launches = %d, credential checks = %d, want at least one of each", len(work), len(check))
			}
			got := envMap(t, work[0].SessionEnv)

			wantAttempt := ""
			if row.Attempt > 1 {
				wantAttempt = strconv.Itoa(row.Attempt - 1)
			}
			want := map[string]string{
				envSessionIssueID:         row.IssueID,
				envSessionIssueIdentifier: row.Identifier,
				envSessionAttempt:         wantAttempt,
				envSessionRule:            row.RuleName,
				envSessionChainID:         row.ChainID,
				envSessionWorkspace:       row.Workspace,
				envSessionAPIURL:          "http://127.0.0.1:7678",
				envSessionPurpose:         sessionPurposeWork,
			}
			for name, value := range want {
				if got[name] != value {
					t.Errorf("work launch %s = %q, want %q (run history row %+v)", name, got[name], value, row)
				}
			}
			if row.RuleName != tt.wantRule || row.ChainID == "" {
				t.Errorf("run history rule/chain = %q/%q, want rule %q and a chain", row.RuleName, row.ChainID, tt.wantRule)
			}
			if got[envSessionDispatchID] == "" {
				t.Errorf("work launch %s is empty", envSessionDispatchID)
			}

			gotCheck := envMap(t, check[0].SessionEnv)
			if gotCheck[envSessionPurpose] != sessionPurposeCredentialCheck {
				t.Errorf("credential check %s = %q, want %q", envSessionPurpose, gotCheck[envSessionPurpose], sessionPurposeCredentialCheck)
			}
			for name, value := range got {
				if name == envSessionPurpose || name == envSessionToken {
					continue
				}
				if gotCheck[name] != value {
					t.Errorf("credential check %s = %q, want the work launch's %q", name, gotCheck[name], value)
				}
			}
		})
	}
}

func TestBuildSessionEnv_Attempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt *int
		want    string
	}{
		{nil, ""},
		{new(1), "1"},
		{new(3), "3"},
	}
	for _, tt := range tests {
		got := envMap(t, buildSessionEnv(sessionEnvParams{Attempt: tt.attempt, Purpose: sessionPurposeWork}))
		if v, ok := got[envSessionAttempt]; !ok || v != tt.want {
			t.Errorf("buildSessionEnv(attempt %v) %s = %q (present %v), want %q", tt.attempt, envSessionAttempt, v, ok, tt.want)
		}
	}
}

func TestBuildSessionEnv_EmptyAPIURLStaysSet(t *testing.T) {
	t.Parallel()

	got := envMap(t, buildSessionEnv(sessionEnvParams{Purpose: sessionPurposeWork}))
	if v, ok := got[envSessionAPIURL]; !ok || v != "" {
		t.Errorf("buildSessionEnv(no server) %s = %q (present %v), want present and empty", envSessionAPIURL, v, ok)
	}
}

// TestRun_SessionTokenPerWorkSession dispatches one issue twice and
// asserts that each work session gets its own session token, that the
// credential checks get none, and that a token stops resolving once its
// run has ended.
func TestRun_SessionTokenPerWorkSession(t *testing.T) {
	t.Parallel()

	h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
	h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1")}
	h.tracker.issues["id-1"] = candidate("id-1", "S-1")
	h.run()
	h.waitForRunHistory("id-1")
	h.o.RefreshFunc()()
	h.waitForSessions(2)
	h.stop()

	var tokens []string
	for _, r := range h.recorders {
		for _, p := range r.started() {
			tokens = append(tokens, envMap(t, p.SessionEnv)[envSessionToken])
		}
		for _, p := range r.verified() {
			if v, ok := envMap(t, p.SessionEnv)[envSessionToken]; ok {
				t.Errorf("credential check carries %s = %q, want none", envSessionToken, v)
			}
		}
	}
	if len(tokens) < 2 {
		t.Fatalf("work sessions = %d, want 2", len(tokens))
	}
	if tokens[0] == "" || tokens[1] == "" || tokens[0] == tokens[1] {
		t.Fatalf("work session tokens = %q, want two different non-empty tokens", tokens[:2])
	}
	if len(tokens[0]) < 22 {
		t.Errorf("session token %q is shorter than 128 bits of base64url", tokens[0])
	}
	if identifier, ok := h.state.SessionTokens.Lookup(tokens[0]); ok {
		t.Errorf("Lookup(first run's token) = %q, true after its run ended, want false", identifier)
	}
}

func TestDispatchIssue_SessionTokenIsIndexedAndMasked(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, 0, nil, AgentTotals{})
	issue := domain.Issue{ID: "id-mask", Identifier: "MASK-1", State: "To Do"}
	got := make(chan sessionIdentity, 1)
	DispatchIssue(context.Background(), state, issue, nil, "", func(ctx context.Context, _ domain.Issue, _ *int) {
		got <- sessionIdentityFrom(ctx)
	})
	state.WorkerWg.Wait()

	token := state.Running[issue.ID].SessionToken
	if token == "" {
		t.Fatal("RunningEntry.SessionToken is empty after DispatchIssue")
	}
	if id := <-got; id.Token != token {
		t.Errorf("worker identity token = %q, want the running entry's %q", id.Token, token)
	}
	if identifier, ok := state.SessionTokens.Lookup(token); !ok || identifier != "MASK-1" {
		t.Errorf("Lookup(token) = %q, %v, want %q, true", identifier, ok, "MASK-1")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redact.ReplaceAttr}))
	logger.Info("agent status", slog.String("message", "my token is "+token))
	if strings.Contains(buf.String(), token) {
		t.Errorf("logged status statement carries the session token: %s", buf.String())
	}
}

// TestSessionToken_DoesNotTrimTruncatedText guards against a live token
// trimming unrelated truncated text: masking a cut text drops a trailing
// fragment that begins a registered value, so a token must not begin
// with a byte such text commonly ends with.
func TestSessionToken_DoesNotTrimTruncatedText(t *testing.T) {
	t.Parallel()

	index := NewSessionTokenIndex()
	// A first random character equal to the prefix byte begins every
	// token, so masking rightly cuts it; draw a token whose first random
	// character is any other.
	token := mintSessionToken()
	for i := 0; i < 64 && strings.HasPrefix(token[len(sessionTokenPrefix):], sessionTokenPrefix); i++ {
		token = mintSessionToken()
	}
	if !strings.HasPrefix(token, sessionTokenPrefix) || len(token) < 1+43 {
		t.Fatalf("token %q, want the prefix and 32 base64url-encoded bytes", token)
	}
	first := len(sessionTokenPrefix)
	text := "cut text ending in the token's first random character " + token[first:first+1]
	// The masker is process-wide, and parallel tests register secrets of
	// their own; one that begins with the same character trims the text
	// whatever this token does. Compare with the result before the token
	// is registered, so only the token's effect is measured.
	before := redact.MaskTruncated(text)

	index.register(token, "id-trim", "TRIM-1")
	defer index.revoke(token)

	if got := redact.MaskTruncated(text); got != before {
		t.Errorf("MaskTruncated(%q) = %q with a live session token, want %q as without it", text, got, before)
	}
}
