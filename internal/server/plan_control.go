package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// PlanControlFunc pulls back or cancels the plan story planKey names. It
// must be safe to call from any goroutine.
type PlanControlFunc func(ctx context.Context, planKey string) orchestrator.ControlReply

type planControlRequest struct {
	PlanKey string `json:"plan_key"`
}

type planControlResponse struct {
	PlanKey string `json:"plan_key"`
	Message string `json:"message,omitempty"`
}

// planControlHandler serves POST /api/v1/plan/pull-back and
// /api/v1/plan/cancel, which take {"plan_key": "..."}.
func (s *Server) planControlHandler(fn func() PlanControlFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		f := fn()
		if f == nil {
			writeErrorJSON(w, s.logger, http.StatusServiceUnavailable, "orchestrator_unavailable", "plan control is not available")
			return
		}
		var req planControlRequest
		if err := decodeControlBody(r, &req); err != nil || strings.TrimSpace(req.PlanKey) == "" {
			writeErrorJSON(w, s.logger, http.StatusBadRequest, "invalid_request", `request body must be {"plan_key": "<key>"}`)
			return
		}
		reply := f(r.Context(), req.PlanKey)
		s.writeControlReply(w, r, start, reply, planControlResponse{PlanKey: req.PlanKey, Message: reply.Message})
	}
}
