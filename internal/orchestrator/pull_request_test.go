package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// calculatorIssue is the issue of the pull request tests.
func calculatorIssue() domain.Issue {
	return domain.Issue{ID: "id-42", Identifier: "ENG-42", Title: "Calculator adds wrong", URL: "https://tracker.example/ENG-42"}
}

// writeSuggestion writes data as the workspace's .sortie/pr.json.
func writeSuggestion(t *testing.T, ws string, data []byte) {
	t.Helper()
	dir := filepath.Join(ws, workspacekit.SortieDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pullRequestSuggestionFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPullRequestTextFor(t *testing.T) {
	t.Parallel()

	defaults := pullRequestText{
		Title: "ENG-42: Calculator adds wrong",
		Body:  "Opened by Sortie for ENG-42.\n\nIssue: https://tracker.example/ENG-42",
	}
	longTitle := strings.Repeat("é", 300)
	tests := []struct {
		name  string
		write func(t *testing.T, ws string)
		want  pullRequestText
	}{
		{
			name: "a suggested title and body",
			write: func(t *testing.T, ws string) {
				writeSuggestion(t, ws, []byte(`{"title": "Fix add()", "body": "Adds a test."}`))
			},
			want: pullRequestText{Title: "Fix add()", Body: "Adds a test."},
		},
		{name: "no file", write: func(*testing.T, string) {}, want: defaults},
		{
			name: "an oversized file",
			write: func(t *testing.T, ws string) {
				body := strings.Repeat("x", pullRequestSuggestionMaxBytes)
				writeSuggestion(t, ws, []byte(`{"title": "Fix add()", "body": "`+body+`"}`))
			},
			want: defaults,
		},
		{
			name: "control characters",
			write: func(t *testing.T, ws string) {
				writeSuggestion(t, ws, []byte(`{"title": "Fix\u001b[31m add()\nnow\u0007", "body": "Line 1\r\n\tLine 2\u0000\u009b"}`))
			},
			want: pullRequestText{Title: "Fix[31m add() now", Body: "Line 1\n\tLine 2"},
		},
		{
			name: "a title cut to 256 characters",
			write: func(t *testing.T, ws string) {
				writeSuggestion(t, ws, []byte(`{"title": "`+longTitle+`", "body": ""}`))
			},
			want: pullRequestText{Title: strings.Repeat("é", 256), Body: ""},
		},
		{name: "malformed JSON", write: func(t *testing.T, ws string) { writeSuggestion(t, ws, []byte(`{"title": `)) }, want: defaults},
		{name: "not an object", write: func(t *testing.T, ws string) { writeSuggestion(t, ws, []byte(`["Fix add()"]`)) }, want: defaults},
		{name: "no body", write: func(t *testing.T, ws string) { writeSuggestion(t, ws, []byte(`{"title": "Fix add()"}`)) }, want: defaults},
		{name: "a non-string title", write: func(t *testing.T, ws string) { writeSuggestion(t, ws, []byte(`{"title": 7, "body": "b"}`)) }, want: defaults},
		{name: "a blank title", write: func(t *testing.T, ws string) { writeSuggestion(t, ws, []byte(`{"title": " \n\u0001", "body": "b"}`)) }, want: defaults},
		{
			name: "a link",
			write: func(t *testing.T, ws string) {
				target := filepath.Join(t.TempDir(), "pr.json")
				if err := os.WriteFile(target, []byte(`{"title": "Linked", "body": "b"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(ws, workspacekit.SortieDir)
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, pullRequestSuggestionFile)); err != nil {
					t.Skipf("symbolic links unavailable: %v", err)
				}
			},
			want: defaults,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := t.TempDir()
			tt.write(t, ws)
			got := pullRequestTextFor(ws, calculatorIssue(), discardLogger())
			if got != tt.want {
				t.Errorf("pullRequestTextFor() = %+q, want %+q", got, tt.want)
			}
			if n := utf8.RuneCountInString(got.Title); n > pullRequestTitleMaxRunes {
				t.Errorf("title has %d characters, want at most %d", n, pullRequestTitleMaxRunes)
			}
		})
	}
}

func TestDefaultPullRequestText(t *testing.T) {
	t.Parallel()

	got := defaultPullRequestText(domain.Issue{Identifier: "ENG-7", Title: "Odd\ttitle\x1b"})
	want := pullRequestText{Title: "ENG-7: Odd title", Body: "Opened by Sortie for ENG-7."}
	if got != want {
		t.Errorf("defaultPullRequestText() = %+q, want %+q", got, want)
	}
}
