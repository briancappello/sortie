package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/render"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/redact"
)

func escalationState() *State {
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.Running["id-1"] = &RunningEntry{
		Identifier: "LIVE-1", Issue: domain.Issue{ID: "id-1", Identifier: "LIVE-1"},
		RuleName: "implement", RetryAttempt: new(2), DispatchID: "d-1", AgentKind: "opencode",
	}
	state.RetryAttempts["id-2"] = &RetryEntry{IssueID: "id-2", Identifier: "LIVE-2", Attempt: 1}
	return state
}

func escalationHarness(t *testing.T, events ...domain.EventType) (*notifierSpy, escalationParams, *bytes.Buffer) {
	t.Helper()
	spy := &notifierSpy{}
	var in route.Inputs
	if len(events) > 0 {
		in.Entries = append(in.Entries, subscribe("webhook", events...))
	}
	var logs bytes.Buffer
	return spy, escalationParams{
		Router: mustRouter(t, nil, spyLookup(spy), in),
		Wg:     &sync.WaitGroup{},
		Ctx:    context.Background(),
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	}, &logs
}

func TestApplyEscalate_DeliversOneEvent(t *testing.T) {
	t.Parallel()

	spy, p, _ := escalationHarness(t, domain.EventSessionEscalated)
	state := escalationState()

	reply := applyEscalate(state, "LIVE-1", EscalationRequest{Message: "Which database should I target?", Kind: "question", URL: "https://viewer.example/s/1"}, reconcileBaseTime, p)
	p.Wg.Wait()

	if reply.Outcome != ControlAccepted {
		t.Fatalf("escalate outcome = %v (%s), want ControlAccepted", reply.Outcome, reply.Message)
	}
	got := spy.notifications()
	if len(got) != 1 {
		t.Fatalf("delivered %d notifications, want 1", len(got))
	}
	n := got[0]
	if n.Envelope.EventType != domain.EventSessionEscalated || n.Envelope.Identifier != "LIVE-1" || n.Envelope.IssueID != "id-1" {
		t.Errorf("envelope = %+v, want session.escalated for LIVE-1", n.Envelope)
	}
	if n.Envelope.Attempt == nil || *n.Envelope.Attempt != 2 {
		t.Errorf("envelope attempt = %v, want 2", n.Envelope.Attempt)
	}
	if n.Envelope.Escalation != (domain.SessionEscalation{Kind: "question", Rule: "implement", URL: "https://viewer.example/s/1"}) {
		t.Errorf("escalation = %+v, want kind question, rule implement, and the URL", n.Envelope.Escalation)
	}
	text := render.Notification(n)
	if n.Message.AgentText != "Which database should I target?" || !strings.Contains(text.Title, "LIVE-1") {
		t.Errorf("message = %+v, rendered title %q, want the text as AgentText and a title naming LIVE-1", n.Message, text.Title)
	}
	if want := "The running session on rule implement asks for a person (question).\nSession: https://viewer.example/s/1"; text.Body != want {
		t.Errorf("rendered body = %q, want %q", text.Body, want)
	}
}

func TestApplyEscalate_Rejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		identifier string
		req        EscalationRequest
		want       ControlOutcome
	}{
		{"empty message", "LIVE-1", EscalationRequest{Message: "  ", Kind: "question"}, ControlInvalid},
		{"unknown kind", "LIVE-1", EscalationRequest{Message: "help", Kind: "panic"}, ControlInvalid},
		{"issue not running", "LIVE-2", EscalationRequest{Message: "help"}, ControlNotRunning},
		{"unknown issue", "NOPE-1", EscalationRequest{Message: "help"}, ControlUnknownIssue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spy, p, _ := escalationHarness(t, domain.EventSessionEscalated)
			reply := applyEscalate(escalationState(), tt.identifier, tt.req, reconcileBaseTime, p)
			p.Wg.Wait()
			if reply.Outcome != tt.want {
				t.Errorf("escalate outcome = %v, want %v", reply.Outcome, tt.want)
			}
			if len(spy.notifications()) != 0 {
				t.Error("a rejected escalation was delivered")
			}
		})
	}
}

func TestApplyEscalate_OnePerIssuePerMinute(t *testing.T) {
	t.Parallel()

	spy, p, _ := escalationHarness(t, domain.EventSessionEscalated)
	state := escalationState()
	req := EscalationRequest{Message: "help", Kind: "question"}

	outcomes := []ControlOutcome{
		applyEscalate(state, "LIVE-1", req, reconcileBaseTime, p).Outcome,
		applyEscalate(state, "LIVE-1", req, reconcileBaseTime.Add(5*time.Second), p).Outcome,
		applyEscalate(state, "LIVE-1", req, reconcileBaseTime.Add(10*time.Second), p).Outcome,
		applyEscalate(state, "LIVE-1", req, reconcileBaseTime.Add(61*time.Second), p).Outcome,
	}
	p.Wg.Wait()

	want := []ControlOutcome{ControlAccepted, ControlRateLimited, ControlRateLimited, ControlAccepted}
	for i := range want {
		if outcomes[i] != want[i] {
			t.Errorf("escalation %d outcome = %v, want %v", i+1, outcomes[i], want[i])
		}
	}
	if got := len(spy.notifications()); got != 2 {
		t.Errorf("delivered %d escalations, want 2 (the first and the one after the window)", got)
	}
}

func TestApplyEscalate_MasksSecrets(t *testing.T) {
	t.Parallel()

	const secret = "escalation-secret-value-4f2d"
	redact.Add("test.escalation", secret)
	spy, p, _ := escalationHarness(t, domain.EventSessionEscalated)

	applyEscalate(escalationState(), "LIVE-1", EscalationRequest{Message: "the key is " + secret, URL: "https://x.example/?k=" + secret}, reconcileBaseTime, p)
	p.Wg.Wait()

	got := spy.notifications()
	if len(got) != 1 {
		t.Fatalf("delivered %d notifications, want 1", len(got))
	}
	for _, text := range []string{got[0].Message.AgentText, got[0].Message.Body, got[0].Envelope.Escalation.URL} {
		if strings.Contains(text, secret) {
			t.Errorf("delivered text %q carries the registered secret", text)
		}
	}
}

func TestApplyEscalate_NoDestinationLogsOnce(t *testing.T) {
	t.Parallel()

	spy, p, logs := escalationHarness(t)
	reply := applyEscalate(escalationState(), "LIVE-1", EscalationRequest{Message: "help"}, reconcileBaseTime, p)
	p.Wg.Wait()

	if reply.Outcome != ControlAccepted {
		t.Errorf("escalate outcome = %v, want ControlAccepted with no destination", reply.Outcome)
	}
	if len(spy.notifications()) != 0 {
		t.Error("an escalation with no subscribed destination was delivered")
	}
	if n := strings.Count(logs.String(), "session escalation had no destination"); n != 1 {
		t.Errorf("log holds %d no-destination records, want 1: %s", n, logs.String())
	}
}

func TestEscalateFunc_ThroughTheEventLoop(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
	h.recorders["kind-a"].adapter.runTurnFn = func(ctx context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
	}
	h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1")}
	h.run()
	h.waitForSessions(1)

	escalate := h.o.EscalateFunc()
	if got := escalate("S-1", EscalationRequest{Message: "help", Kind: "question"}).Outcome; got != ControlAccepted {
		t.Errorf("escalate(S-1) = %v, want ControlAccepted", got)
	}
	if got := escalate("S-1", EscalationRequest{Message: "again"}).Outcome; got != ControlRateLimited {
		t.Errorf("second escalate(S-1) = %v, want ControlRateLimited", got)
	}
	close(release)
	h.stop()
}
