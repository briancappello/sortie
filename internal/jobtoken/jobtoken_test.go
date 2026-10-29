package jobtoken

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const (
	flowGit Flow = "git"
	flowLLM Flow = "llm"
)

func mint(t *testing.T, i *Issuer, job string) map[Flow]Token {
	t.Helper()
	toks, err := i.OnLeaseGrant(job)
	if err != nil {
		t.Fatalf("OnLeaseGrant(%q): %v", job, err)
	}
	return toks
}

func TestTakingAJobMintsOneTokenPerFlow(t *testing.T) {
	t.Parallel()
	i := NewIssuer(flowGit, flowLLM)
	toks := mint(t, i, "job-a")
	if len(toks) != 2 {
		t.Fatalf("got %d tokens, want 2", len(toks))
	}
	if toks[flowGit].Reveal() == "" || toks[flowGit].Reveal() == toks[flowLLM].Reveal() {
		t.Fatal("flows must get distinct non-empty tokens")
	}
	for f, tok := range toks {
		g, err := i.Check(tok.Reveal(), "job-a", f)
		if err != nil || g.Flow != f || g.JobID != "job-a" || g.Attempt != 1 {
			t.Fatalf("Check(%s) = %+v, %v", f, g, err)
		}
	}
	other := mint(t, i, "job-b")
	if other[flowGit].Reveal() == toks[flowGit].Reveal() {
		t.Fatal("two jobs share a token")
	}
}

func TestDataPlaneRequestsAreChecked(t *testing.T) {
	t.Parallel()
	i := NewIssuer(flowGit, flowLLM)
	a := mint(t, i, "job-a")
	mint(t, i, "job-b")

	reached := 0
	h := i.Require(flowGit, func(r *http.Request) string { return r.PathValue("job") },
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	mux := http.NewServeMux()
	mux.Handle("/git/{job}", h)

	cases := []struct {
		name, job, auth string
		want            int
	}{
		{"own job and flow", "job-a", "Bearer " + a[flowGit].Reveal(), http.StatusOK},
		{"refused for another job", "job-b", "Bearer " + a[flowGit].Reveal(), http.StatusForbidden},
		{"refused for another flow", "job-a", "Bearer " + a[flowLLM].Reveal(), http.StatusForbidden},
		{"missing token", "job-a", "", http.StatusUnauthorized},
		{"unknown token", "job-a", "Bearer nope", http.StatusUnauthorized},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/git/"+c.job, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if reached != 1 {
		t.Fatalf("handler reached %d times, want 1", reached)
	}
	if _, err := i.Check(a[flowGit].Reveal(), "job-b", flowGit); !errors.Is(err, ErrWrongJob) {
		t.Fatalf("Check for another job = %v, want ErrWrongJob", err)
	}
}

func TestTokensEndWithTheAttempt(t *testing.T) {
	t.Parallel()
	i := NewIssuer(flowGit)
	first := mint(t, i, "job-a")
	i.OnLeaseEnd("job-a")
	if _, err := i.Check(first[flowGit].Reveal(), "job-a", flowGit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("after end: %v, want ErrInvalid", err)
	}

	second := mint(t, i, "job-a")
	third := mint(t, i, "job-a")
	if _, err := i.Check(second[flowGit].Reveal(), "job-a", flowGit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("earlier attempt token: %v, want ErrInvalid", err)
	}
	g, err := i.Check(third[flowGit].Reveal(), "job-a", flowGit)
	if err != nil || g.Attempt != 3 {
		t.Fatalf("live attempt: %+v, %v", g, err)
	}
}

func TestTokensAreNotKeptOrShownInClear(t *testing.T) {
	t.Parallel()
	i := NewIssuer(flowGit)
	tok := mint(t, i, "job-a")[flowGit]
	secret := tok.Reveal()

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("minted", slog.Any("token", tok))
	js, _ := json.Marshal(map[string]Token{"t": tok})
	shown := []string{
		fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok),
		buf.String(), string(js),
	}
	for _, s := range shown {
		if strings.Contains(s, secret) {
			t.Fatalf("secret shown in clear: %q", s)
		}
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if strings.Contains(fmt.Sprintf("%#v", i.grants), secret) {
		t.Fatal("issuer keeps the secret in clear")
	}
	if _, ok := i.grants[hash(secret)]; !ok {
		t.Fatal("issuer does not keep the digest")
	}
}

func TestProxiesAskWhatATokenGrants(t *testing.T) {
	t.Parallel()
	i := NewIssuer(flowGit, flowLLM)
	tok := mint(t, i, "job-a")[flowLLM]
	srv := httptest.NewServer(i.IntrospectHandler())
	defer srv.Close()

	ask := func(auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}

	code, body := ask("Bearer " + tok.Reveal())
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if strings.Contains(body, tok.Reveal()) {
		t.Fatal("introspection echoes the token")
	}
	var g Grant
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatal(err)
	}
	if want := (Grant{JobID: "job-a", Attempt: 1, Flow: flowLLM}); !reflect.DeepEqual(g, want) {
		t.Fatalf("grant %+v, want %+v", g, want)
	}

	if code, _ := ask("Bearer bogus"); code != http.StatusUnauthorized {
		t.Fatalf("unknown token status %d", code)
	}
	i.OnLeaseEnd("job-a")
	if code, _ := ask("Bearer " + tok.Reveal()); code != http.StatusUnauthorized {
		t.Fatalf("ended token status %d", code)
	}
}
