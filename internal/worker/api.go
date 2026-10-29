package worker

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Handler serves the worker API. Workers dial Sortie; Sortie never
// dials a worker.
//
//	POST /api/v1/workers/checkin          body Worker
//	POST /api/v1/workers/{id}/poll        200 {job, lease} or 204
//	POST /api/v1/workers/{id}/jobs/{job}/renew
//	POST /api/v1/workers/{id}/jobs/{job}/stop
//	GET  /api/v1/workers                  State
func Handler(m *Matcher) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/workers/checkin", func(w http.ResponseWriter, r *http.Request) {
		var wk Worker
		if err := json.NewDecoder(r.Body).Decode(&wk); err != nil || wk.ID == "" {
			http.Error(w, "invalid worker", http.StatusBadRequest)
			return
		}
		m.CheckIn(wk)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/workers/{id}/poll", func(w http.ResponseWriter, r *http.Request) {
		j, ls, ok, err := m.Poll(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		reply(w, map[string]any{"job": j, "lease": ls})
	})
	mux.HandleFunc("POST /api/v1/workers/{id}/jobs/{job}/renew", func(w http.ResponseWriter, r *http.Request) {
		ls, err := m.Renew(r.PathValue("job"), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, ls)
	})
	mux.HandleFunc("POST /api/v1/workers/{id}/jobs/{job}/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Stop(r.PathValue("job"), r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/workers", func(w http.ResponseWriter, r *http.Request) {
		reply(w, m.Snapshot())
	})
	return mux
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrUnknownWorker):
		code = http.StatusNotFound
	case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrUnknownLease):
		code = http.StatusConflict
	}
	http.Error(w, err.Error(), code)
}
