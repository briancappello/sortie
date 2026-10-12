package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// pullsServer fakes GitHub's pull request list and create routes for
// org/app. open holds the list response; create answers a POST.
type pullsServer struct {
	t      *testing.T
	mu     sync.Mutex
	open   string
	create func(w http.ResponseWriter, body createPullRequestBody)

	listQueries []string
	posts       []createPullRequestBody
}

func (s *pullsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path != "/repos/org/app/pulls" {
		s.t.Errorf("request %s %s, want /repos/org/app/pulls", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listQueries = append(s.listQueries, r.URL.RawQuery)
		q := r.URL.Query()
		if q.Get("state") != "open" || q.Get("head") != "org:sortie/ENG-42" {
			s.t.Errorf("list query = %q, want open pull requests with head org:sortie/ENG-42", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(s.open))
	case http.MethodPost:
		var body createPullRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("decode create body: %v", err)
		}
		s.posts = append(s.posts, body)
		s.create(w, body)
	default:
		s.t.Errorf("unexpected method %s", r.Method)
	}
}

func TestEnsurePullRequest(t *testing.T) {
	t.Parallel()

	const existing = `[{"number":7,"html_url":"https://github.com/org/app/pull/7","head":{"ref":"sortie/ENG-42"}}]`
	noCreate := func(t *testing.T) func(http.ResponseWriter, createPullRequestBody) {
		return func(w http.ResponseWriter, _ createPullRequestBody) {
			t.Error("pull request created, want none")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}

	t.Run("an open pull request is found unchanged", func(t *testing.T) {
		t.Parallel()
		s := &pullsServer{t: t, open: existing, create: noCreate(t)}
		srv := httptest.NewServer(s)
		defer srv.Close()

		got, err := newTestSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "org", "app", "sortie/ENG-42", "main", "New title", "New body")
		if err != nil {
			t.Fatalf("EnsurePullRequest() error = %v", err)
		}
		want := domain.PullRequest{Number: 7, URL: "https://github.com/org/app/pull/7"}
		if got != want {
			t.Errorf("EnsurePullRequest() = %+v, want %+v", got, want)
		}
		if len(s.posts) != 0 {
			t.Errorf("posts = %+v, want none", s.posts)
		}
	})

	t.Run("a missing pull request is created", func(t *testing.T) {
		t.Parallel()
		s := &pullsServer{t: t, open: `[]`, create: func(w http.ResponseWriter, _ createPullRequestBody) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":8,"html_url":"https://github.com/org/app/pull/8","head":{"ref":"sortie/ENG-42"}}`))
		}}
		srv := httptest.NewServer(s)
		defer srv.Close()

		got, err := newTestSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "org", "app", "sortie/ENG-42", "main", "Fix add()", "Adds a test.")
		if err != nil {
			t.Fatalf("EnsurePullRequest() error = %v", err)
		}
		want := domain.PullRequest{Number: 8, URL: "https://github.com/org/app/pull/8", Created: true}
		if got != want {
			t.Errorf("EnsurePullRequest() = %+v, want %+v", got, want)
		}
		wantPost := createPullRequestBody{Title: "Fix add()", Head: "sortie/ENG-42", Base: "main", Body: "Adds a test."}
		if len(s.posts) != 1 || s.posts[0] != wantPost {
			t.Errorf("posts = %+v, want one %+v", s.posts, wantPost)
		}
	})

	t.Run("a pull request created concurrently is found after a 422", func(t *testing.T) {
		t.Parallel()
		var s *pullsServer
		s = &pullsServer{t: t, open: `[]`, create: func(w http.ResponseWriter, _ createPullRequestBody) {
			s.open = existing
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for org:sortie/ENG-42."}]}`))
		}}
		srv := httptest.NewServer(s)
		defer srv.Close()

		got, err := newTestSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "org", "app", "sortie/ENG-42", "main", "t", "b")
		if err != nil {
			t.Fatalf("EnsurePullRequest() error = %v", err)
		}
		if got.Number != 7 || got.Created {
			t.Errorf("EnsurePullRequest() = %+v, want pull request 7, not created", got)
		}
		if len(s.listQueries) != 2 {
			t.Errorf("list calls = %d, want 2", len(s.listQueries))
		}
	})

	t.Run("a 422 that no open pull request explains is returned", func(t *testing.T) {
		t.Parallel()
		s := &pullsServer{t: t, open: `[]`, create: func(w http.ResponseWriter, _ createPullRequestBody) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"No commits between main and sortie/ENG-42"}]}`))
		}}
		srv := httptest.NewServer(s)
		defer srv.Close()

		_, err := newTestSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "org", "app", "sortie/ENG-42", "main", "t", "b")
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
		if !strings.Contains(err.Error(), "No commits") {
			t.Errorf("error = %v, want the provider's reason", err)
		}
	})

	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"a token without write permission", http.StatusForbidden, `{"message":"Resource not accessible by personal access token"}`},
		{"a token that can read but is hidden from writes", http.StatusNotFound, `{"message":"Not Found"}`},
	} {
		t.Run(tt.name+" fails with an auth error naming the scope", func(t *testing.T) {
			t.Parallel()
			s := &pullsServer{t: t, open: `[]`, create: func(w http.ResponseWriter, _ createPullRequestBody) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}}
			srv := httptest.NewServer(s)
			defer srv.Close()

			_, err := newTestSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "org", "app", "sortie/ENG-42", "main", "t", "b")
			assertSCMErrorKind(t, err, domain.ErrSCMAuth)
			if !strings.Contains(err.Error(), "repo") || !strings.Contains(err.Error(), "Pull requests: write") {
				t.Errorf("error = %v, want it to name the repo scope and the Pull requests: write permission", err)
			}
		})
	}

	t.Run("a rate-limited create stays an API error", func(t *testing.T) {
		t.Parallel()
		s := &pullsServer{t: t, open: `[]`, create: func(w http.ResponseWriter, _ createPullRequestBody) {
			w.Header().Set("X-Ratelimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		}}
		srv := httptest.NewServer(s)
		defer srv.Close()

		_, err := newTestSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "org", "app", "sortie/ENG-42", "main", "t", "b")
		assertSCMErrorKind(t, err, domain.ErrSCMAPI)
	})
}
