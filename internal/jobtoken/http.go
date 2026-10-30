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

// IntrospectHandler answers a proxy asking what the bearer token it was
// shown grants: 200 with the [Grant] as JSON, or 401 when the token is
// unknown or its attempt ended. The response never echoes the token.
func (i *Issuer) IntrospectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		g, err := i.Lookup(bearer(r))
		if err != nil {
			http.Error(w, "invalid or ended job token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g)
	})
}
