package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// BrokerReportFunc keeps the address the broker of the running issue
// named by identifier reported. It must be safe to call from any
// goroutine.
type BrokerReportFunc func(identifier string, b orchestrator.BrokerAddress) orchestrator.ControlReply

// BrokerReadFunc returns the address and viewer token of the running
// issue's broker. It must be safe to call from any goroutine.
type BrokerReadFunc func(identifier string) orchestrator.ControlReply

type brokerReport struct {
	HTTPURL     string `json:"http_url"`
	ViewerToken string `json:"viewer_token"`
}

type brokerResponse struct {
	IssueIdentifier string `json:"issue_identifier"`
	HTTPURL         string `json:"http_url"`
	ViewerToken     string `json:"viewer_token,omitempty"`
}

// routeBroker serves /api/v1/{identifier}/broker. POST is the broker's
// report, accepted with the session token of that issue's run. GET returns
// the address with the viewer token, and needs the control token.
func (s *Server) routeBroker(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.requireControl(scopeSession, s.handleBrokerReport)(w, r)
	case http.MethodGet:
		s.requireControl(scopeOperator, s.handleBrokerRead)(w, r)
	default:
		s.methodNotAllowed(w, r, http.MethodGet+", "+http.MethodPost)
	}
}

func (s *Server) handleBrokerReport(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.brokerReportFn == nil {
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable, "orchestrator_unavailable", "broker reports are not available")
		return
	}
	var req brokerReport
	if err := decodeControlBody(r, &req); err != nil || strings.TrimSpace(req.HTTPURL) == "" || req.ViewerToken == "" {
		writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_request", `request body must be {"http_url": "...", "viewer_token": "..."}`)
		return
	}
	identifier := r.PathValue("identifier")
	reply := s.brokerReportFn(identifier, orchestrator.BrokerAddress{URL: req.HTTPURL, ViewerToken: req.ViewerToken})
	s.writeControlReply(w, r, start, reply, brokerResponse{IssueIdentifier: identifier, HTTPURL: req.HTTPURL})
}

func (s *Server) handleBrokerRead(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.brokerReadFn == nil {
		writeErrorJSON(w, s.logger, http.StatusServiceUnavailable, "orchestrator_unavailable", "broker reads are not available")
		return
	}
	identifier := r.PathValue("identifier")
	reply := s.brokerReadFn(identifier)
	s.writeControlReply(w, r, start, reply, brokerResponse{IssueIdentifier: identifier, HTTPURL: reply.Broker.URL, ViewerToken: reply.Broker.ViewerToken})
}
