package orchestrator

import (
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/redact"
)

func TestBrokerReport_KeptMaskedAndRead(t *testing.T) {
	t.Parallel()
	st := NewState(1000, 2, 0, nil, AgentTotals{})
	st.Running["i1"] = &RunningEntry{Identifier: "LIVE-1"}
	token := "viewer-token-d41d8cd98f00b204"

	if r := applyBrokerReport(st, "LIVE-1", BrokerAddress{URL: "http://127.0.0.1:41001", ViewerToken: token}, discardLogger()); r.Outcome != ControlDone {
		t.Fatalf("report = %+v, want done", r)
	}
	if got := redact.Mask("token " + token); got == "token "+token {
		t.Error("the viewer token is not masked")
	}
	if r := readBroker(st, "LIVE-1"); r.Broker.ViewerToken != token || r.Broker.URL != "http://127.0.0.1:41001" {
		t.Errorf("read = %+v", r.Broker)
	}
	snap := RuntimeSnapshot(st, time.Now())
	for _, e := range snap.Running {
		if e.BrokerURL != "http://127.0.0.1:41001" {
			t.Errorf("snapshot broker URL = %q", e.BrokerURL)
		}
	}

	for _, bad := range []BrokerAddress{{URL: "file:///etc/passwd", ViewerToken: "x"}, {URL: "http://h:1"}} {
		if r := applyBrokerReport(st, "LIVE-1", bad, discardLogger()); r.Outcome != ControlInvalid {
			t.Errorf("report %+v = %+v, want invalid", bad, r)
		}
	}
	if r := applyBrokerReport(st, "NOPE", BrokerAddress{URL: "http://h:1", ViewerToken: "t"}, discardLogger()); r.Outcome != ControlUnknownIssue {
		t.Errorf("unknown issue = %+v", r)
	}
	if r := readBroker(st, "NOPE"); r.Outcome != ControlUnknownIssue {
		t.Errorf("read unknown = %+v", r)
	}
}

// When the run ends, the entry and its broker go with it.
func TestBrokerReport_ForgottenWhenTheRunEnds(t *testing.T) {
	t.Parallel()
	f := newHopExitFixture(t)
	f.state.Running[hopExitIssueID].BrokerURL = "http://127.0.0.1:41002"
	f.state.Running[hopExitIssueID].BrokerViewerToken = "viewer-token-e3b0c44298fc1c14"
	f.exit()
	if r := readBroker(f.state, f.result.Identifier); r.Outcome == ControlDone {
		t.Errorf("broker still readable after the run ended: %+v", r)
	}
}
