package jobtoken

import (
	"encoding/json"
	"net/http"
	"strings"
)

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// Require wraps a data-plane handler of flow so every request must carry
// a bearer token minted for flow and for the job jobOf names in the
// request. A missing, ended or foreign token gets 401 or 403 and never
// reaches next.
func (i *Issuer) Require(flow Flow, jobOf func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := i.Check(bearer(r), jobOf(r), flow)
		switch err {
		case nil:
			next.ServeHTTP(w, r)
		case ErrInvalid:
			w.Header().Set("WWW-Authenticate", `Bearer realm="sortie-job"`)
			http.Error(w, "invalid or ended job token", http.StatusUnauthorized)
		default:
			http.Error(w, "job token does not grant this request", http.StatusForbidden)
		}
	})
}

// maxIntrospectBody bounds the JSON body of an introspection request.
const maxIntrospectBody = 4 << 10

type introspectRequest struct {
	Token string `json:"token"`
}

type introspectReply struct {
	Job     string `json:"job"`
	Attempt uint64 `json:"attempt"`
	Flow    Flow   `json:"flow"`
}

// IntrospectHandler answers a proxy asking what a token it was shown
// grants. The request body is {"token"}; the answer is 200 with
// {"job", "attempt", "flow"} for a live token, and 404 for any other
// or for an unreadable body. The answer never echoes the token. The
// caller authenticates the proxy before this handler.
func (i *Issuer) IntrospectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var req introspectRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIntrospectBody)).Decode(&req); err != nil {
			http.Error(w, "unknown or ended job token", http.StatusNotFound)
			return
		}
		g, err := i.Lookup(req.Token)
		if err != nil {
			http.Error(w, "unknown or ended job token", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(introspectReply{Job: g.JobID, Attempt: g.Attempt, Flow: g.Flow})
	})
}
