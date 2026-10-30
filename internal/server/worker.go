package server

import (
	"net/http"

	"github.com/sortie-ai/sortie/internal/worker"
)

// workerRoutes are the worker API routes of protocol 1, all served by
// [Server.routeWorker].
var workerRoutes = []string{
	"/api/v1/worker/checkin",
	"/api/v1/worker/poll",
	"/api/v1/worker/jobs/{job}/attempts/{attempt}/heartbeat",
	"/api/v1/worker/jobs/{job}/attempts/{attempt}/report",
}

// routeWorker serves the worker API while the remote environment kind
// is selected, and 404 otherwise.
func (s *Server) routeWorker(w http.ResponseWriter, r *http.Request) {
	var m *worker.Matcher
	if s.workersFn != nil {
		m = s.workersFn()
	}
	if m == nil {
		writeErrorJSON(w, s.logger, http.StatusNotFound, "not_found",
			"the worker API is served only under environment.kind remote")
		return
	}
	header := ""
	if s.workerHeaderFn != nil {
		header = s.workerHeaderFn()
	}
	worker.Handler(m, header).ServeHTTP(w, r)
}

// workersState returns the workers and leases GET /api/v1/state lists,
// or nil while the remote environment kind is not selected.
func (s *Server) workersState() *worker.State {
	if s.workersFn == nil {
		return nil
	}
	m := s.workersFn()
	if m == nil {
		return nil
	}
	return new(m.Snapshot())
}
