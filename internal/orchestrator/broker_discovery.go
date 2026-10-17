package orchestrator

import (
	"fmt"
	"log/slog"
	"net/url"

	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
)

// BrokerAddress is the HTTP address and viewer token of a run's broker.
type BrokerAddress struct {
	URL         string
	ViewerToken string
}

// applyBrokerReport keeps the address the run's broker reported, on the
// event loop. The viewer token is registered for masking.
func applyBrokerReport(state *State, identifier string, b BrokerAddress, log *slog.Logger) ControlReply {
	issueID, entry, known := findIssue(state, identifier)
	switch {
	case !known:
		return ControlReply{Outcome: ControlUnknownIssue, Message: fmt.Sprintf("issue %q is not known", identifier)}
	case entry == nil:
		return ControlReply{Outcome: ControlNotRunning, Message: fmt.Sprintf("issue %q has no running worker", identifier)}
	}
	u, err := url.Parse(b.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || b.ViewerToken == "" {
		return ControlReply{Outcome: ControlInvalid, Message: "a broker report needs an http(s) URL and a viewer token"}
	}
	if entry.BrokerViewerToken != "" && entry.BrokerViewerToken != b.ViewerToken {
		redact.Remove(entry.BrokerViewerToken)
	}
	redact.Add("broker.viewer_token", b.ViewerToken)
	entry.BrokerURL, entry.BrokerViewerToken = b.URL, b.ViewerToken
	logging.WithIssue(log, issueID, entry.Identifier).Info("broker reported", slog.String("broker_url", b.URL))
	return ControlReply{Outcome: ControlDone}
}

// readBroker returns the address of the running issue's broker.
func readBroker(state *State, identifier string) ControlReply {
	_, entry, known := findIssue(state, identifier)
	switch {
	case !known || entry == nil:
		return ControlReply{Outcome: ControlUnknownIssue, Message: fmt.Sprintf("issue %q has no running worker", identifier)}
	case entry.BrokerURL == "":
		return ControlReply{Outcome: ControlUnknownIssue, Message: fmt.Sprintf("the broker of %q has not reported", identifier)}
	}
	return ControlReply{Outcome: ControlDone, Broker: BrokerAddress{URL: entry.BrokerURL, ViewerToken: entry.BrokerViewerToken}}
}

// BrokerReportFunc returns the function the HTTP server calls when a run's
// broker reports its address. Safe to call from any goroutine.
func (o *Orchestrator) BrokerReportFunc() func(identifier string, b BrokerAddress) ControlReply {
	return func(identifier string, b BrokerAddress) ControlReply {
		return o.sendControl(controlRequest{Kind: controlBrokerReport, Identifier: identifier, Broker: b})
	}
}

// BrokerReadFunc returns the function the HTTP server calls to read a
// running issue's broker address and viewer token. Safe to call from any
// goroutine.
func (o *Orchestrator) BrokerReadFunc() func(identifier string) ControlReply {
	return func(identifier string) ControlReply {
		return o.sendControl(controlRequest{Kind: controlBrokerRead, Identifier: identifier})
	}
}
