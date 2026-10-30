package worker

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// DefaultIdentityHeader is the request header that names the worker
// when worker.identity_header is unset.
const DefaultIdentityHeader = "X-Sortie-Worker"

// Prefix is the path every worker API route sits under.
const Prefix = "/api/v1/worker/"

// Handler serves worker API protocol 1. Workers dial Sortie; Sortie
// never dials a worker. The worker's identity is the value of the
// identityHeader request header, never a path segment.
//
//	POST /api/v1/worker/checkin                                  body Declaration
//	POST /api/v1/worker/poll                                     200 Offer or 204
//	POST /api/v1/worker/jobs/{job}/attempts/{attempt}/heartbeat  200 lease
//	POST /api/v1/worker/jobs/{job}/attempts/{attempt}/report     body {outcome, error}
func Handler(m *Matcher, identityHeader string) http.Handler {
	if identityHeader == "" {
		identityHeader = DefaultIdentityHeader
	}
	mux := http.NewServeMux()
	identified := func(h func(w http.ResponseWriter, r *http.Request, id string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(identityHeader)
			if id == "" {
				problem(w, http.StatusUnauthorized, "missing_identity", "the "+identityHeader+" header names no worker", nil)
				return
			}
			h(w, r, id)
		}
	}
	mux.HandleFunc("POST /api/v1/worker/checkin", identified(func(w http.ResponseWriter, r *http.Request, id string) {
		d, missing, err := decodeDeclaration(r)
		switch {
		case err != nil:
			problem(w, http.StatusBadRequest, "invalid_checkin", err.Error(), nil)
			return
		case len(missing) > 0:
			problem(w, http.StatusBadRequest, "invalid_checkin", "check-in declaration is missing fields", missing)
			return
		case d.Protocol != Protocol:
			problem(w, http.StatusBadRequest, "unsupported_protocol", "this Sortie serves worker API protocol "+strconv.Itoa(Protocol), nil)
			return
		}
		m.CheckIn(id, d)
		reply(w, http.StatusOK, map[string]any{"protocol": Protocol, "worker": id})
	}))
	mux.HandleFunc("POST /api/v1/worker/poll", identified(func(w http.ResponseWriter, r *http.Request, id string) {
		offer, ok, err := m.Poll(id)
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		reply(w, http.StatusOK, offer)
	}))
	mux.HandleFunc("POST /api/v1/worker/jobs/{job}/attempts/{attempt}/heartbeat", identified(func(w http.ResponseWriter, r *http.Request, id string) {
		attempt, err := strconv.Atoi(r.PathValue("attempt"))
		if err != nil {
			problem(w, http.StatusBadRequest, "invalid_attempt", "attempt must be an integer", nil)
			return
		}
		ls, err := m.Heartbeat(id, r.PathValue("job"), attempt)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, http.StatusOK, ls)
	}))
	mux.HandleFunc("POST /api/v1/worker/jobs/{job}/attempts/{attempt}/report", identified(func(w http.ResponseWriter, r *http.Request, id string) {
		attempt, err := strconv.Atoi(r.PathValue("attempt"))
		if err != nil {
			problem(w, http.StatusBadRequest, "invalid_attempt", "attempt must be an integer", nil)
			return
		}
		var body struct {
			Outcome string `json:"outcome"`
			Error   string `json:"error"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Outcome != "succeeded" && body.Outcome != "failed") {
			problem(w, http.StatusBadRequest, "invalid_report", `outcome must be "succeeded" or "failed"`, nil)
			return
		}
		out := Outcome{Succeeded: body.Outcome == "succeeded", Error: body.Error}
		if err := m.Report(id, r.PathValue("job"), attempt, out); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// decodeDeclaration reads a check-in body and names each field it
// lacks.
func decodeDeclaration(r *http.Request) (Declaration, []string, error) {
	var raw struct {
		Protocol     *int      `json:"protocol"`
		Capabilities *[]string `json:"capabilities"`
		Resources    *struct {
			CPUs      *int   `json:"cpus"`
			MemoryMiB *int64 `json:"memory_mib"`
			DiskMiB   *int64 `json:"disk_mib"`
			RunSlots  *int   `json:"run_slots"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return Declaration{}, nil, errors.New("check-in body is not a JSON object")
	}
	var d Declaration
	var missing []string
	if raw.Protocol == nil {
		missing = append(missing, "protocol")
	} else {
		d.Protocol = *raw.Protocol
	}
	if raw.Capabilities == nil {
		missing = append(missing, "capabilities")
	} else {
		d.Capabilities = *raw.Capabilities
	}
	if raw.Resources == nil {
		missing = append(missing, "resources.cpus", "resources.memory_mib", "resources.disk_mib", "resources.run_slots")
		return d, missing, nil
	}
	res := raw.Resources
	if res.CPUs == nil {
		missing = append(missing, "resources.cpus")
	} else {
		d.Resources.CPUs = *res.CPUs
	}
	if res.MemoryMiB == nil {
		missing = append(missing, "resources.memory_mib")
	} else {
		d.Resources.MemoryMiB = *res.MemoryMiB
	}
	if res.DiskMiB == nil {
		missing = append(missing, "resources.disk_mib")
	} else {
		d.Resources.DiskMiB = *res.DiskMiB
	}
	if res.RunSlots == nil {
		missing = append(missing, "resources.run_slots")
	} else {
		d.Resources.RunSlots = *res.RunSlots
	}
	return d, missing, nil
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, code int, kind, msg string, missing []string) {
	body := map[string]any{"error": map[string]any{"code": kind, "message": msg}}
	if missing != nil {
		body["missing"] = missing
	}
	reply(w, code, body)
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownWorker):
		problem(w, http.StatusNotFound, "unknown_worker", "check in before polling", nil)
	case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrUnknownLease):
		problem(w, http.StatusConflict, "lease_lost", err.Error(), nil)
	default:
		problem(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}
