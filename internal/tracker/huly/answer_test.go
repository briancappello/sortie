package huly

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/parkanswer"
	"github.com/sortie-ai/sortie/internal/prompt"
)

const answerStatement = "Which repository should the daemon live in?"

// Scenario: an answer resumes the story. Sortie posts the answer through
// the adapter, and the comments the next run reads back from Huly hold it
// in the park's window, so the run's prompt carries the statement and the
// answer.
func TestParkAnswerResumesStory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := seedWorkspace()
	a := newTestAdapter(t, f)
	parkedAt := time.Now().UTC().Truncate(time.Second)

	posted, err := parkanswer.Post(ctx, a, "HULY-1", true, parkanswer.Answer{Text: "agentic-orchestration", Author: "Serghei"})
	if err != nil {
		t.Fatalf("parkanswer.Post() error = %v", err)
	}
	if txs := f.txsOf(classChatMessage); len(txs) != 1 || txs[0]["attachedTo"] != "issue1" {
		t.Fatalf("message txs = %+v, want one comment on issue1", txs)
	}

	comments, err := a.FetchIssueComments(ctx, "issue1")
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}
	var sincePark []domain.Comment
	for _, c := range comments {
		if created, err := c.CreatedTime(); err != nil || !created.Before(parkedAt) {
			sincePark = append(sincePark, c)
		}
	}
	if len(sincePark) != 1 {
		t.Fatalf("comments since the park = %+v, want the answer only", sincePark)
	}
	// Huly keeps each line of a posted comment as a paragraph, so the blank
	// line between the attribution and the text reads back as two.
	if got, want := sincePark[0].Body, "Serghei answered through Sortie:\n\n\n\nagentic-orchestration"; got != want || posted.Comment() != "Serghei answered through Sortie:\n\nagentic-orchestration" {
		t.Errorf("answer comment reads back as %q (posted %q), want %q", got, posted.Comment(), want)
	}

	tmpl, err := prompt.Parse("{{ .park.statement }}\n{{ range .park.comments }}{{ .body }}{{ end }}", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := tmpl.Render(map[string]any{}, nil, prompt.RunContext{}, prompt.WithPark(prompt.ParkContext{
		Statement: answerStatement,
		Gesture:   "answered",
		Comments:  domain.CommentTemplateMaps(sincePark),
	}))
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if want := answerStatement + "\nSerghei answered through Sortie:\n\n\n\nagentic-orchestration"; rendered != want {
		t.Errorf("prompt = %q, want %q", rendered, want)
	}
}

// Scenario: not parked. Sortie refuses the answer and Huly receives no
// write.
func TestParkAnswerNotParkedPostsNothing(t *testing.T) {
	t.Parallel()

	f := seedWorkspace()
	a := newTestAdapter(t, f)

	_, err := parkanswer.Post(context.Background(), a, "HULY-3", false, parkanswer.Answer{Text: "agentic-orchestration", Author: "Serghei"})
	if !errors.Is(err, parkanswer.ErrNotParked) {
		t.Fatalf("parkanswer.Post() error = %v, want ErrNotParked", err)
	}
	if len(f.txs) != 0 {
		t.Errorf("transactions = %+v, want none", f.txs)
	}
	comments, err := a.FetchIssueComments(context.Background(), "issue3")
	if err != nil || len(comments) != 0 {
		t.Errorf("FetchIssueComments(issue3) = %+v, %v, want no comments", comments, err)
	}
}
