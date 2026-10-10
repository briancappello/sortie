package markup

import (
	"encoding/json"
	"testing"
)

func TestRenderMarkdownHulyNodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			"person reference renders as a mention",
			para(`{"type":"text","text":"ask "},{"type":"reference","attrs":{"id":"p1","objectclass":"contact:class:Person","label":"Brian"}}`),
			"ask @Brian\n",
		},
		{
			"issue reference renders its label",
			para(`{"type":"reference","attrs":{"id":"i1","objectclass":"tracker:class:Issue","label":"HULY-7"}}`),
			"HULY-7\n",
		},
		{
			"emoji",
			para(`{"type":"text","text":"ok "},{"type":"emoji","attrs":{"emoji":"👍"}}`),
			"ok 👍\n",
		},
		{
			"mermaid",
			`{"type":"doc","content":[{"type":"mermaid","content":[{"type":"text","text":"graph TD;"}]}]}`,
			"```mermaid\ngraph TD;\n```\n",
		},
		{
			"todo list carries checkbox state",
			`{"type":"doc","content":[{"type":"todoList","content":[` +
				`{"type":"todoItem","attrs":{"checked":true},"content":[{"type":"paragraph","content":[{"type":"text","text":"done"}]}]},` +
				`{"type":"todoItem","attrs":{"checked":false},"content":[{"type":"paragraph","content":[{"type":"text","text":"open"}]}]}]}]}`,
			"- [x] done\n- [ ] open\n",
		},
		{
			"ordered list uses huly order attribute",
			`{"type":"doc","content":[{"type":"orderedList","attrs":{"order":5},"content":[` +
				`{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"e"}]}]}]}]}`,
			"5. e\n",
		},
		{
			"uploaded image without url is named",
			`{"type":"doc","content":[{"type":"image","attrs":{"file-id":"abc","alt":"diagram"}}]}`,
			"[image: diagram]\n",
		},
		{
			"highlight mark is dropped",
			para(`{"type":"text","text":"hot","marks":[{"type":"highlight"},{"type":"bold"}]}`),
			"**hot**\n",
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

func TestToMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"blank", "  \n", ""},
		{"not json is returned unchanged", "plain words", "plain words"},
		{"document", para(`{"type":"text","text":"hi","marks":[{"type":"bold"}]}`), "**hi**"},
		{
			"render failure falls back to plain text",
			`{"type":"doc","content":[{"type":"bulletList","content":[{"type":"paragraph","content":[{"type":"text","text":"stray"}]}]}]}`,
			"stray",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ToMarkdown(tc.in); got != tc.want {
				t.Errorf("ToMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromText(t *testing.T) {
	t.Parallel()

	got := FromText("line one\n\nline *two*")
	want := `{"type":"doc","content":[` +
		`{"type":"paragraph","content":[{"type":"text","text":"line one"}]},` +
		`{"type":"paragraph"},` +
		`{"type":"paragraph","content":[{"type":"text","text":"line *two*"}]}]}`

	if got != want {
		t.Errorf("FromText() =\n%s\nwant\n%s", got, want)
	}

	if md := ToMarkdown(got); md != "line one\n\n\n\nline \\*two\\*" {
		t.Errorf("ToMarkdown(FromText()) = %q", md)
	}
}

func TestFromTextWithLiteral(t *testing.T) {
	t.Parallel()

	got := FromTextWithLiteral("Session stopped.", "see `main.go` [x](y)")

	var n Node
	if err := json.Unmarshal([]byte(got), &n); err != nil {
		t.Fatalf("FromTextWithLiteral() is not valid json: %v", err)
	}

	if len(n.Content) != 2 {
		t.Fatalf("content length = %d, want 2", len(n.Content))
	}

	block := n.Content[1]
	if block.Type != NodeCodeBlock || len(block.Content) != 1 || *block.Content[0].Text != "see `main.go` [x](y)" {
		t.Errorf("literal block = %+v, want one code block holding the literal verbatim", block)
	}
}
