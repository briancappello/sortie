// Package parkanswer posts a person's answer to a parked issue as a tracker
// comment attributed to that person. The orchestrator decides whether the
// issue is parked and releases the park; this package only shapes and posts
// the comment, so every tracker adapter receives the same text.
package parkanswer

import (
	"context"
	"errors"
	"strings"

	"github.com/sortie-ai/sortie/internal/redact"
)

// ErrNotParked reports that the issue an answer names holds no park, so
// nothing was posted.
var ErrNotParked = errors.New("the issue is not parked")

// ErrInvalid reports an answer with no text or no author.
var ErrInvalid = errors.New("text and author must not be empty")

// Answer is a person's reply to the stop statement of a parked issue.
type Answer struct {
	Text   string
	Author string
}

// Normalize trims both fields and masks registered secrets in them. It
// returns [ErrInvalid] when either field holds no non-space character.
func (a Answer) Normalize() (Answer, error) {
	text := strings.TrimSpace(redact.Mask(strings.TrimSpace(a.Text)))
	author := strings.Join(strings.Fields(redact.Mask(a.Author)), " ")
	if text == "" || author == "" {
		return Answer{}, ErrInvalid
	}
	return Answer{Text: text, Author: author}, nil
}

// Comment returns the comment body that carries the answer: one line naming
// the author, a blank line, then the text as written. Sortie posts with its
// own tracker credentials, so the first line is what attributes the answer.
func (a Answer) Comment() string {
	return a.Author + " answered through Sortie:\n\n" + a.Text
}

// Commenter is the tracker write [Post] needs.
type Commenter interface {
	CommentIssue(ctx context.Context, issueID, text string) error
}

// Post normalizes a and posts its comment on issueID. It posts nothing and
// returns [ErrNotParked] when parked is false, and [ErrInvalid] for an
// answer [Answer.Normalize] refuses. Any other error is the tracker's.
func Post(ctx context.Context, tracker Commenter, issueID string, parked bool, a Answer) (Answer, error) {
	answer, err := a.Normalize()
	if err != nil {
		return Answer{}, err
	}
	if !parked {
		return Answer{}, ErrNotParked
	}
	if err := tracker.CommentIssue(ctx, issueID, answer.Comment()); err != nil {
		return Answer{}, err
	}
	return answer, nil
}
