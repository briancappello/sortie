package parkanswer

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/redact"
)

type recordingCommenter struct {
	issueIDs []string
	texts    []string
	err      error
}

func (r *recordingCommenter) CommentIssue(_ context.Context, issueID, text string) error {
	r.issueIDs = append(r.issueIDs, issueID)
	r.texts = append(r.texts, text)
	return r.err
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      Answer
		want    Answer
		wantErr error
	}{
		{"trims both fields", Answer{Text: "\n agentic-orchestration \n", Author: "  Serghei  Iakovlev "}, Answer{Text: "agentic-orchestration", Author: "Serghei Iakovlev"}, nil},
		{"keeps the lines of the text", Answer{Text: "first\n\nsecond", Author: "Ada"}, Answer{Text: "first\n\nsecond", Author: "Ada"}, nil},
		{"author on one line", Answer{Text: "yes", Author: "Ada\nLovelace"}, Answer{Text: "yes", Author: "Ada Lovelace"}, nil},
		{"blank text", Answer{Text: " \t\n", Author: "Ada"}, Answer{}, ErrInvalid},
		{"blank author", Answer{Text: "yes", Author: " "}, Answer{}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.in.Normalize()
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Errorf("Normalize(%+v) = %+v, %v, want %+v, %v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestNormalizeMasksSecrets(t *testing.T) {
	const secret = "parkanswer-test-secret-value-9f3a"
	redact.Add("parkanswer test", secret)
	t.Cleanup(func() { redact.Remove(secret) })

	got, err := Answer{Text: "the key is " + secret, Author: "Ada"}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.Text == "the key is "+secret {
		t.Errorf("Normalize text = %q, want the registered secret masked", got.Text)
	}
}

func TestComment(t *testing.T) {
	t.Parallel()

	got := Answer{Text: "agentic-orchestration", Author: "Serghei"}.Comment()
	if want := "Serghei answered through Sortie:\n\nagentic-orchestration"; got != want {
		t.Errorf("Comment() = %q, want %q", got, want)
	}
}

func TestPost(t *testing.T) {
	t.Parallel()

	answer := Answer{Text: " agentic-orchestration ", Author: "Serghei"}
	tests := []struct {
		name      string
		parked    bool
		answer    Answer
		postErr   error
		wantErr   error
		wantPosts int
	}{
		{"parked issue", true, answer, nil, nil, 1},
		{"issue that is not parked", false, answer, nil, ErrNotParked, 0},
		{"invalid answer", true, Answer{Text: "", Author: "Serghei"}, nil, ErrInvalid, 0},
		{"tracker fails", true, answer, errTracker, errTracker, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &recordingCommenter{err: tt.postErr}
			got, err := Post(context.Background(), c, "id-25", tt.parked, tt.answer)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Post() error = %v, want %v", err, tt.wantErr)
			}
			if len(c.texts) != tt.wantPosts {
				t.Fatalf("comments posted = %q, want %d", c.texts, tt.wantPosts)
			}
			if tt.wantErr != nil {
				return
			}
			if c.issueIDs[0] != "id-25" || c.texts[0] != "Serghei answered through Sortie:\n\nagentic-orchestration" {
				t.Errorf("posted %q on %q, want the attributed answer on id-25", c.texts[0], c.issueIDs[0])
			}
			if got != (Answer{Text: "agentic-orchestration", Author: "Serghei"}) {
				t.Errorf("Post() answer = %+v, want the normalized answer", got)
			}
		})
	}
}

var errTracker = errors.New("tracker unavailable")
