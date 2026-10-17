package markup

import (
	"strings"
	"testing"
)

func TestFromMarkdownRoundTrip(t *testing.T) {
	t.Parallel()

	md := strings.Join([]string{
		"Use the ledger",
		"for totals.",
		"",
		"## Acceptance criteria",
		"",
		"- An invoice exists.",
		"- Totals use `currency`.",
		"",
		"1. first",
		"2. second",
		"",
		"```go",
		"x := 1",
		"",
		"y := 2",
		"```",
	}, "\n")

	got := ToMarkdown(FromMarkdown(md))
	for _, want := range []string{
		"Use the ledger for totals.",
		"## Acceptance criteria",
		"- An invoice exists.",
		"`currency`",
		"1. first",
		"2. second",
		"```go\nx := 1\n\ny := 2\n```",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("round trip lost %q:\n%s", want, got)
		}
	}
}

func TestFromMarkdownNodes(t *testing.T) {
	t.Parallel()

	doc, err := Parse(FromMarkdown("# Title\n\nText with `code` and more.\n\n- one"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Type != NodeDoc || len(doc.Content) != 3 {
		t.Fatalf("doc = %+v, want three blocks", doc)
	}
	if h := doc.Content[0]; h.Type != NodeHeading || string(h.Attrs) != `{"level":1}` {
		t.Errorf("heading = %+v", h)
	}
	p := doc.Content[1]
	if len(p.Content) != 3 || p.Content[1].Marks[0].Type != MarkCode || *p.Content[1].Text != "code" {
		t.Errorf("paragraph = %+v, want text, a code span, text", p)
	}
	if l := doc.Content[2]; l.Type != NodeBulletList || l.Content[0].Type != NodeListItem {
		t.Errorf("list = %+v", l)
	}
}

func TestFromMarkdownEmptyAndUnclosed(t *testing.T) {
	t.Parallel()

	if got := FromMarkdown(""); got != `{"type":"doc"}` {
		t.Errorf("FromMarkdown(\"\") = %s", got)
	}
	got := ToMarkdown(FromMarkdown("```\nopen fence\nno close"))
	if !strings.Contains(got, "open fence\nno close") {
		t.Errorf("unclosed fence lost its body: %q", got)
	}
	d, err := Parse(FromMarkdown("a `b"))
	if err != nil || *d.Content[0].Content[0].Text != "a `b" {
		t.Errorf("unmatched backtick = %+v, %v; want the text kept literally", d, err)
	}
}
