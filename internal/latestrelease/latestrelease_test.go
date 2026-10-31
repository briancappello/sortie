package latestrelease

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func releasesServer(t *testing.T, status int, body string, gotAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/tool/releases" {
			http.NotFound(w, r)
			return
		}
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGitHub_HighestStableRelease(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	body := `[
		{"tag_name": "v2.0.0", "draft": true, "prerelease": false},
		{"tag_name": "v1.40.0", "draft": false, "prerelease": true},
		{"tag_name": "v1.39.0-rc.1", "draft": false, "prerelease": false},
		{"tag_name": "nightly", "draft": false, "prerelease": false},
		{"tag_name": "v1.32.4", "draft": false, "prerelease": false},
		{"tag_name": "1.33.1", "draft": false, "prerelease": false},
		{"tag_name": "v1.9.9", "draft": false, "prerelease": false}
	]`
	srv := releasesServer(t, http.StatusOK, body, nil)
	got, err := GitHub(context.Background(), srv.Client(), srv.URL, "owner/tool")
	if err != nil || got != "1.33.1" {
		t.Errorf("GitHub() = %q, %v, want 1.33.1", got, err)
	}
}

func TestGitHub_SendsTheToken(t *testing.T) {
	var auth string
	srv := releasesServer(t, http.StatusOK, `[{"tag_name": "v1.0.0"}]`, &auth)

	t.Setenv("GH_TOKEN", "test-token")
	if _, err := GitHub(context.Background(), srv.Client(), srv.URL, "owner/tool"); err != nil {
		t.Fatalf("GitHub() error = %v", err)
	}
	if auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the bearer token", auth)
	}

	t.Setenv("GH_TOKEN", "")
	if _, err := GitHub(context.Background(), srv.Client(), srv.URL, "owner/tool"); err != nil {
		t.Fatalf("GitHub() error = %v", err)
	}
	if auth != "" {
		t.Errorf("Authorization = %q without GH_TOKEN, want none", auth)
	}
}

func TestGitHub_FailuresNameTheSource(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	for name, tt := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"http error":        {http.StatusForbidden, `{"message": "API rate limit exceeded"}`, "HTTP 403"},
		"not json":          {http.StatusOK, `<html>`, "parse "},
		"no stable release": {http.StatusOK, `[{"tag_name": "v1.0.0", "prerelease": true}]`, "no stable release"},
	} {
		srv := releasesServer(t, tt.status, tt.body, nil)
		_, err := GitHub(context.Background(), srv.Client(), srv.URL, "owner/tool")
		source := srv.URL + "/repos/owner/tool/releases"
		if err == nil || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: GitHub() error = %v, want it to name %s and %q", name, err, source, tt.want)
		}
	}

	srv := releasesServer(t, http.StatusOK, `[]`, nil)
	srv.Close()
	if _, err := GitHub(context.Background(), srv.Client(), srv.URL, "owner/tool"); err == nil || !strings.Contains(err.Error(), "fetch "+srv.URL) {
		t.Errorf("unreachable: GitHub() error = %v, want it to name the source", err)
	}
}

func TestBehind(t *testing.T) {
	t.Parallel()

	if msg := Behind("smolvm", "1.26.1", "1.32.4"); msg != "smolvm is behind: ours 1.26.1, latest 1.32.4" {
		t.Errorf("Behind() = %q", msg)
	}
	for _, ours := range []string{"1.32.4", "1.33.0"} {
		if msg := Behind("smolvm", ours, "1.32.4"); msg != "" {
			t.Errorf("Behind(%s) = %q, want none", ours, msg)
		}
	}
}

func TestParseGoModules(t *testing.T) {
	t.Parallel()

	out := []byte(`{"Path": "example.com/main", "Main": true}
{"Path": "example.com/direct", "Version": "v1.2.0", "Update": {"Version": "v1.3.0"}}
{"Path": "example.com/indirect", "Version": "v0.1.0", "Indirect": true}
{"Path": "example.com/graph-only", "Version": "v0.0.1", "Indirect": true, "Update": {"Version": "v0.0.2"}}
{"Path": "example.com/local", "Version": "v0.0.0", "Replace": {"Path": "../local"}}
`)
	required := map[string]bool{"example.com/direct": true, "example.com/indirect": true, "example.com/local": true}
	mods, err := parseGoModules(out, "src", required)
	if err != nil {
		t.Fatalf("parseGoModules() error = %v", err)
	}
	want := []Module{
		{Path: "example.com/direct", Version: "v1.2.0", Latest: "v1.3.0"},
		{Path: "example.com/indirect", Version: "v0.1.0", Indirect: true},
	}
	if len(mods) != len(want) || mods[0] != want[0] || mods[1] != want[1] {
		t.Errorf("parseGoModules() = %+v, want %+v", mods, want)
	}

	required["example.com/missing"] = true
	if _, err := parseGoModules(out, "src", required); err == nil || !strings.Contains(err.Error(), "src") || !strings.Contains(err.Error(), "example.com/missing") {
		t.Errorf("parseGoModules() error = %v, want it to name the source and the missing module", err)
	}
	if _, err := parseGoModules([]byte(`{"Path": "example.com/direct", "Error": {"Err": "no network"}}`), "src", required); err == nil || !strings.Contains(err.Error(), "no network") {
		t.Errorf("parseGoModules() error = %v, want the module error", err)
	}
	if _, err := parseGoModules([]byte(`{`), "src", required); err == nil || !strings.Contains(err.Error(), "parse src") {
		t.Errorf("parseGoModules() error = %v, want a parse error naming the source", err)
	}
}
