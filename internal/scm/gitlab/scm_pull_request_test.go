package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// mergeRequestsServer fakes GitLab's merge request list and create
// routes for the project group/sub/app. open holds the list response;
// create answers a POST.
type mergeRequestsServer struct {
	t      *testing.T
	mu     sync.Mutex
	open   string
	create func(w http.ResponseWriter)

	lists int
	posts []gitlabCreateMergeRequest
}

func (s *mergeRequestsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.EscapedPath() != "/api/v4/projects/group%2Fsub%2Fapp/merge_requests" {
		s.t.Errorf("request %s %s, want the project's merge requests", r.Method, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.lists++
		q := r.URL.Query()
		if q.Get("state") != "opened" || q.Get("source_branch") != "sortie/ENG-42" {
			s.t.Errorf("list query = %q, want opened merge requests from sortie/ENG-42", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(s.open))
	case http.MethodPost:
		var body gitlabCreateMergeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Errorf("decode create body: %v", err)
		}
		s.posts = append(s.posts, body)
		s.create(w)
	default:
		s.t.Errorf("unexpected method %s", r.Method)
	}
}

func TestEnsurePullRequest(t *testing.T) {
	t.Parallel()

	const existing = `[` +
		`{"iid":3,"web_url":"https://gitlab.com/fork/app/-/merge_requests/3","source_branch":"sortie/ENG-42","source_project_id":99,"target_project_id":5},` +
		`{"iid":4,"web_url":"https://gitlab.com/group/sub/app/-/merge_requests/4","source_branch":"sortie/ENG-42","source_project_id":5,"target_project_id":5}]`
	ensure := func(t *testing.T, s *mergeRequestsServer, title, body string) (domain.PullRequest, error) {
		t.Helper()
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		return mustSCMAdapter(t, srv.URL).EnsurePullRequest(t.Context(), "group/sub", "app", "sortie/ENG-42", "main", title, body)
	}

	t.Run("an open merge request is found unchanged, never a fork's", func(t *testing.T) {
		t.Parallel()
		s := &mergeRequestsServer{t: t, open: existing, create: func(w http.ResponseWriter) {
			t.Error("merge request created, want none")
			w.WriteHeader(http.StatusInternalServerError)
		}}
		got, err := ensure(t, s, "New title", "New body")
		if err != nil {
			t.Fatalf("EnsurePullRequest() error = %v", err)
		}
		want := domain.PullRequest{Number: 4, URL: "https://gitlab.com/group/sub/app/-/merge_requests/4"}
		if got != want {
			t.Errorf("EnsurePullRequest() = %+v, want %+v", got, want)
		}
	})

	t.Run("a missing merge request is created", func(t *testing.T) {
		t.Parallel()
		s := &mergeRequestsServer{t: t, open: `[]`, create: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"iid":5,"web_url":"https://gitlab.com/group/sub/app/-/merge_requests/5","source_branch":"sortie/ENG-42","source_project_id":5,"target_project_id":5}`))
		}}
		got, err := ensure(t, s, "Fix add()", "Adds a test.")
		if err != nil {
			t.Fatalf("EnsurePullRequest() error = %v", err)
		}
		want := domain.PullRequest{Number: 5, URL: "https://gitlab.com/group/sub/app/-/merge_requests/5", Created: true}
		if got != want {
			t.Errorf("EnsurePullRequest() = %+v, want %+v", got, want)
		}
		wantPost := gitlabCreateMergeRequest{SourceBranch: "sortie/ENG-42", TargetBranch: "main", Title: "Fix add()", Description: "Adds a test."}
		if len(s.posts) != 1 || s.posts[0] != wantPost {
			t.Errorf("posts = %+v, want one %+v", s.posts, wantPost)
		}
	})

	t.Run("a merge request created concurrently is found after a 409", func(t *testing.T) {
		t.Parallel()
		var s *mergeRequestsServer
		s = &mergeRequestsServer{t: t, open: `[]`, create: func(w http.ResponseWriter) {
			s.open = existing
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":["Another open merge request already exists for this source branch: !4"]}`))
		}}
		got, err := ensure(t, s, "t", "b")
		if err != nil {
			t.Fatalf("EnsurePullRequest() error = %v", err)
		}
		if got.Number != 4 || got.Created || s.lists != 2 {
			t.Errorf("EnsurePullRequest() = %+v after %d lists, want merge request 4, not created, after 2", got, s.lists)
		}
	})

	t.Run("a token that cannot create fails with an auth error naming the scope", func(t *testing.T) {
		t.Parallel()
		s := &mergeRequestsServer{t: t, open: `[]`, create: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api"}`))
		}}
		_, err := ensure(t, s, "t", "b")
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)
		if !strings.Contains(err.Error(), "api scope") {
			t.Errorf("error = %v, want it to name the api scope", err)
		}
	})
}
