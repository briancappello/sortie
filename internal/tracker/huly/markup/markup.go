// Package markup converts between Huly's rich-text markup, a ProseMirror
// (Tiptap) JSON document, and the plain text and Markdown that Sortie
// exchanges with a tracker. The Markdown renderer is derived from Probo's
// MIT-licensed prosemirror package; see LICENSE.probo in this directory.
package markup

import (
	"encoding/json"
	"strings"
)

// ToMarkdown renders a serialized Huly markup document as Markdown. An
// empty input renders as the empty string. A document the renderer cannot
// handle falls back to its plain text, and input that is not markup JSON is
// returned unchanged, so a description is never lost to a rendering fault.
func ToMarkdown(serialized string) string {
	if strings.TrimSpace(serialized) == "" {
		return ""
	}

	doc, err := Parse(serialized)
	if err != nil {
		return serialized
	}

	out, err := RenderMarkdown(doc)
	if err != nil {
		return PlainText(doc)
	}

	return strings.TrimRight(out, "\n")
}

// PlainText returns the text content of n with blocks separated by line
// feeds.
func PlainText(n Node) string {
	var b strings.Builder
	writePlain(&b, n)

	return strings.TrimSpace(b.String())
}

func writePlain(b *strings.Builder, n Node) {
	switch {
	case n.Text != nil:
		b.WriteString(*n.Text)
	case n.Type == NodeHardBreak:
		b.WriteByte('\n')
	case n.Type == NodeReference:
		b.WriteString(n.stringAttr("label"))
	case n.Type == NodeEmoji:
		b.WriteString(n.stringAttr("emoji"))
	}

	for _, child := range n.Content {
		writePlain(b, child)
	}

	if !isInline(n.Type) && n.Type != NodeDoc {
		b.WriteByte('\n')
	}
}

// FromText builds a serialized markup document holding text as paragraphs,
// one per line. Text is stored verbatim, so Markdown syntax in it shows as
// written.
func FromText(text string) string {
	return serialize(doc(textParagraphs(text)...))
}

// FromTextWithLiteral builds a serialized markup document holding text as
// paragraphs followed by one code block that holds literal verbatim, so the
// tracker shows literal as written and acts on nothing in it.
func FromTextWithLiteral(text, literal string) string {
	content := textParagraphs(text)
	content = append(content, Node{
		Type:    NodeCodeBlock,
		Content: []Node{textNode(literal)},
	})

	return serialize(doc(content...))
}

func textParagraphs(text string) []Node {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	paragraphs := make([]Node, 0, len(lines))

	for _, line := range lines {
		p := Node{Type: NodeParagraph}
		if line != "" {
			p.Content = []Node{textNode(line)}
		}

		paragraphs = append(paragraphs, p)
	}

	return paragraphs
}

func textNode(s string) Node {
	return Node{Type: NodeText, Text: &s}
}

func doc(content ...Node) Node {
	return Node{Type: NodeDoc, Content: content}
}

func serialize(n Node) string {
	// Marshaling a tree of strings, slices and raw messages cannot fail.
	raw, _ := json.Marshal(n)

	return string(raw)
}
