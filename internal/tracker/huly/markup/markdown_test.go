// Copyright (c) 2026 Probo Inc <hello@probo.com>.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//
// Derived from go.probo.inc/probo/pkg/prosemirror (markdown_renderer_test.go
// at commit a89724c2d519732aa13caa72331f03d8b3f80976). Modified for Sortie:
// rewritten as a table over the standard testing package, and the cases whose
// behavior Sortie changed (unknown nodes and marks, table cells holding
// blocks) assert the new behavior. testdata/document.json and document.md
// come from the same commit; the two table rows of document.md whose cells
// hold lists were changed to the plain-text fallback.

package markup

import (
	"os"
	"strings"
	"testing"
)

func renderJSON(t *testing.T, raw string) string {
	t.Helper()

	n, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	got, err := RenderMarkdown(n)
	if err != nil {
		t.Fatalf("RenderMarkdown() error = %v", err)
	}

	return got
}

func para(inline string) string {
	return `{"type":"doc","content":[{"type":"paragraph","content":[` + inline + `]}]}`
}

func TestRenderMarkdownDocumentFixture(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/document.json")
	if err != nil {
		t.Fatal(err)
	}

	want, err := os.ReadFile("testdata/document.md")
	if err != nil {
		t.Fatal(err)
	}

	if got := renderJSON(t, string(raw)); got != string(want) {
		t.Errorf("RenderMarkdown(document.json) mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty doc", `{"type":"doc"}`, ""},
		{"empty paragraph", `{"type":"doc","content":[{"type":"paragraph"}]}`, ""},
		{"paragraph", para(`{"type":"text","text":"Hello world"}`), "Hello world\n"},
		{
			"heading levels",
			`{"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Title"}]}]}`,
			"## Title\n",
		},
		{
			"heading level out of range is clamped",
			`{"type":"doc","content":[{"type":"heading","attrs":{"level":9},"content":[{"type":"text","text":"Deep"}]}]}`,
			"###### Deep\n",
		},
		{"bold", para(`{"type":"text","text":"bold","marks":[{"type":"bold"}]}`), "**bold**\n"},
		{"italic", para(`{"type":"text","text":"it","marks":[{"type":"italic"}]}`), "*it*\n"},
		{
			"italic trailing space moves outside the mark",
			para(`{"type":"text","text":"it ","marks":[{"type":"italic"}]},{"type":"text","text":"after"}`),
			"*it* after\n",
		},
		{"strike", para(`{"type":"text","text":"gone","marks":[{"type":"strike"}]}`), "~~gone~~\n"},
		{"underline", para(`{"type":"text","text":"u","marks":[{"type":"underline"}]}`), "<u>u</u>\n"},
		{"inline code", para(`{"type":"text","text":"x := 1","marks":[{"type":"code"}]}`), "`x := 1`\n"},
		{
			"inline code with backtick",
			para(`{"type":"text","text":"a` + "`" + `b","marks":[{"type":"code"}]}`),
			"`` a`b ``\n",
		},
		{
			"link",
			para(`{"type":"text","text":"site","marks":[{"type":"link","attrs":{"href":"https://example.com"}}]}`),
			"[site](https://example.com)\n",
		},
		{
			"link with dangerous href is neutralized",
			para(`{"type":"text","text":"x","marks":[{"type":"link","attrs":{"href":"javascript:alert(1)"}}]}`),
			"[x](#)\n",
		},
		{
			"code block with language",
			`{"type":"doc","content":[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println()"}]}]}`,
			"```go\nfmt.Println()\n```\n",
		},
		{
			"code block containing a fence gets a longer fence",
			`{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"` + "```" + `"}]}]}`,
			"````\n```\n````\n",
		},
		{"horizontal rule", `{"type":"doc","content":[{"type":"horizontalRule"}]}`, "---\n"},
		{
			"hard break",
			para(`{"type":"text","text":"a"},{"type":"hardBreak"},{"type":"text","text":"b"}`),
			"a\\\nb\n",
		},
		{
			"image",
			`{"type":"doc","content":[{"type":"image","attrs":{"src":"https://example.com/a.png","alt":"A"}}]}`,
			"![A](https://example.com/a.png)\n",
		},
		{
			"bullet list",
			`{"type":"doc","content":[{"type":"bulletList","content":[` +
				`{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},` +
				`{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}]}]}`,
			"- one\n- two\n",
		},
		{
			"ordered list with start",
			`{"type":"doc","content":[{"type":"orderedList","attrs":{"start":3},"content":[` +
				`{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"c"}]}]}]}]}`,
			"3. c\n",
		},
		{
			"blockquote",
			`{"type":"doc","content":[{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"q"}]}]}]}`,
			"> q\n",
		},
		{
			"gfm table",
			`{"type":"doc","content":[{"type":"table","content":[` +
				`{"type":"tableRow","content":[{"type":"tableHeader","content":[{"type":"paragraph","content":[{"type":"text","text":"Name"}]}]}]},` +
				`{"type":"tableRow","content":[{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"Alice"}]}]}]}]}]}`,
			"| Name |\n| --- |\n| Alice |\n",
		},
		{
			"table cell holding blocks falls back to plain text",
			`{"type":"doc","content":[{"type":"table","content":[` +
				`{"type":"tableRow","content":[{"type":"tableHeader","content":[{"type":"paragraph","content":[{"type":"text","text":"H"}]}]}]},` +
				`{"type":"tableRow","content":[{"type":"tableCell","content":[{"type":"bulletList","content":[` +
				`{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item"}]}]}]}]}]}]}]}`,
			"| H |\n| --- |\n| item |\n",
		},
		{
			"unknown node renders its content",
			`{"type":"doc","content":[{"type":"mystery","content":[{"type":"paragraph","content":[{"type":"text","text":"kept"}]}]}]}`,
			"kept\n",
		},
		{
			"unknown mark is dropped",
			para(`{"type":"text","text":"red","marks":[{"type":"textColor","attrs":{"color":"red"}}]}`),
			"red\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := renderJSON(t, tc.raw); got != tc.want {
				t.Errorf("RenderMarkdown() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderMarkdownEscaping(t *testing.T) {
	t.Parallel()

	got := renderJSON(t, para(`{"type":"text","text":"*b* _i_ [l] ~s~ |p| <h>"}`))

	for _, want := range []string{`\*b\*`, `\_i\_`, `\[l\]`, `\~s\~`, `\|p\|`, `\<h>`} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderMarkdown() = %q, want it to contain %q", got, want)
		}
	}
}

func TestRenderMarkdownTextNil(t *testing.T) {
	t.Parallel()

	_, err := RenderMarkdown(Node{Type: NodeDoc, Content: []Node{{
		Type:    NodeParagraph,
		Content: []Node{{Type: NodeText}},
	}}})
	if err == nil {
		t.Fatal("RenderMarkdown() error = nil, want error for a text node without text")
	}
}
