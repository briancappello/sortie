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
// Derived from go.probo.inc/probo/pkg/prosemirror (markdown_renderer.go at
// commit a89724c2d519732aa13caa72331f03d8b3f80976). Modified for Sortie:
// unknown nodes render their content and unknown marks are dropped instead of
// failing the document, Huly's reference, emoji, mermaid, todo and task nodes
// are rendered, and table cells holding block content fall back to plain text
// instead of HTML.

package markup

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// RenderMarkdown renders a ProseMirror document node tree to a Markdown string.
func RenderMarkdown(node Node) (string, error) {
	r := &mdRenderer{
		buf:         &bytes.Buffer{},
		atLineStart: true,
	}
	if err := r.renderNode(node); err != nil {
		return "", err
	}

	out := strings.TrimRight(r.buf.String(), "\n")
	if out != "" {
		out += "\n"
	}

	return out, nil
}

type mdRenderer struct {
	buf         *bytes.Buffer
	prefix      string
	tight       bool
	atLineStart bool
}

func (r *mdRenderer) ensurePrefix() {
	if r.atLineStart {
		r.buf.WriteString(r.prefix)
		r.atLineStart = false
	}
}

func (r *mdRenderer) newLine() {
	r.buf.WriteByte('\n')
	r.atLineStart = true
}

func (r *mdRenderer) renderNode(n Node) error {
	switch n.Type {
	case NodeDoc:
		return r.renderBlocks(n.Content)
	case NodeParagraph:
		r.ensurePrefix()

		if err := r.renderInline(n.Content); err != nil {
			return err
		}

		r.newLine()
	case NodeHeading:
		attrs, err := n.headingAttrs()
		if err != nil {
			return fmt.Errorf("cannot render heading node: %w", err)
		}

		level := min(max(attrs.Level, 1), 6)

		r.ensurePrefix()

		for range level {
			r.buf.WriteByte('#')
		}

		r.buf.WriteByte(' ')

		if err := r.renderInline(n.Content); err != nil {
			return err
		}

		r.newLine()
	case NodeBlockquote:
		oldPrefix := r.prefix

		r.prefix += "> "
		if err := r.renderBlocks(n.Content); err != nil {
			r.prefix = oldPrefix
			return err
		}

		r.prefix = oldPrefix
	case NodeCodeBlock:
		attrs, err := n.codeBlockAttrs()
		if err != nil {
			return fmt.Errorf("cannot render code block node: %w", err)
		}

		language := ""
		if attrs.Language != nil {
			language = *attrs.Language
		}

		r.renderFenced(language, collectText(n.Content))
	case NodeMermaid:
		r.renderFenced("mermaid", collectText(n.Content))
	case NodeHorizontalRule:
		r.ensurePrefix()
		r.buf.WriteString("---")
		r.newLine()
	case NodeHardBreak:
		r.buf.WriteByte('\\')
		r.newLine()
	case NodeText:
		return r.renderText(n)
	case NodeImage:
		attrs, err := n.imageAttrs()
		if err != nil {
			return fmt.Errorf("cannot render image node: %w", err)
		}

		r.ensurePrefix()

		src := safeImageSrc(attrs.Src)
		if src == "" {
			// Huly stores uploaded images by blob id with no URL; the
			// agent cannot fetch them, so name the image instead.
			r.buf.WriteString("[image")
			if attrs.Alt != nil && *attrs.Alt != "" {
				r.buf.WriteString(": ")
				r.buf.WriteString(escapeMarkdown(*attrs.Alt))
			}
			r.buf.WriteByte(']')

			return nil
		}

		r.buf.WriteString("![")

		if attrs.Alt != nil {
			r.buf.WriteString(escapeMarkdown(*attrs.Alt))
		}

		r.buf.WriteString("](")
		r.buf.WriteString(src)

		if attrs.Title != nil {
			r.buf.WriteString(` "`)
			r.buf.WriteString(strings.ReplaceAll(*attrs.Title, `"`, `\"`))
			r.buf.WriteByte('"')
		}

		r.buf.WriteByte(')')
	case NodeReference:
		r.ensurePrefix()

		label := n.stringAttr("label")
		if strings.HasPrefix(n.stringAttr("objectclass"), "contact:") && !strings.HasPrefix(label, "@") {
			label = "@" + label
		}

		r.buf.WriteString(escapeMarkdown(label))
	case NodeEmoji:
		r.ensurePrefix()
		r.buf.WriteString(n.stringAttr("emoji"))
	case NodeBulletList:
		return r.renderBulletList(n, nil)
	case NodeTodoList, NodeTaskList:
		return r.renderBulletList(n, checkboxMarker)
	case NodeOrderedList:
		return r.renderOrderedList(n)
	case NodeTable:
		return r.renderTable(n)
	default:
		// List items, table parts outside their container, and every node
		// type this renderer does not know render their content, so one
		// unfamiliar node never loses the rest of the document.
		return r.renderContainer(n)
	}

	return nil
}

// renderContainer renders a node by its content alone: as blocks when any
// child is a block node, and inline otherwise.
func (r *mdRenderer) renderContainer(n Node) error {
	if n.Text != nil {
		return r.renderText(n)
	}

	for _, child := range n.Content {
		if !isInline(child.Type) {
			return r.renderBlocks(n.Content)
		}
	}

	if len(n.Content) == 0 {
		return nil
	}

	r.ensurePrefix()

	return r.renderInline(n.Content)
}

func isInline(t NodeType) bool {
	switch t {
	case NodeText, NodeHardBreak, NodeImage, NodeReference, NodeEmoji:
		return true
	default:
		return false
	}
}

func (r *mdRenderer) renderFenced(language, code string) {
	fence := chooseFence(code)

	r.ensurePrefix()
	r.buf.WriteString(fence)
	r.buf.WriteString(language)
	r.newLine()

	for line := range strings.SplitSeq(code, "\n") {
		r.ensurePrefix()
		r.buf.WriteString(line)
		r.newLine()
	}

	r.ensurePrefix()
	r.buf.WriteString(fence)
	r.newLine()
}

func (r *mdRenderer) renderBlocks(nodes []Node) error {
	for i, n := range nodes {
		if i > 0 && !r.tight {
			r.ensurePrefix()
			r.newLine()
		}

		if err := r.renderNode(n); err != nil {
			return err
		}
	}

	return nil
}

func (r *mdRenderer) renderInline(nodes []Node) error {
	for _, n := range nodes {
		if err := r.renderNode(n); err != nil {
			return err
		}
	}

	return nil
}

func (r *mdRenderer) renderText(n Node) error {
	if n.Text == nil {
		return fmt.Errorf("cannot render text node: text is nil")
	}

	r.ensurePrefix()

	marks := knownMarks(n.Marks)
	if len(marks) == 0 {
		r.buf.WriteString(escapeMarkdown(*n.Text))
		return nil
	}

	if slices.ContainsFunc(marks, func(m Mark) bool { return m.Type == MarkCode }) {
		return r.renderCodeText(*n.Text, marks)
	}

	text := *n.Text

	var needsTrim bool

	for _, m := range marks {
		switch m.Type {
		case MarkStrong, MarkEm, MarkStrike:
			needsTrim = true
		}
	}

	var leading, trailing string

	if needsTrim {
		origLen := len(text)
		text = strings.TrimLeft(text, " ")
		leading = strings.Repeat(" ", origLen-len(text))
		origLen = len(text)
		text = strings.TrimRight(text, " ")
		trailing = strings.Repeat(" ", origLen-len(text))
	}

	if text == "" {
		r.buf.WriteString(leading)
		r.buf.WriteString(trailing)

		return nil
	}

	r.buf.WriteString(leading)

	for _, m := range marks {
		r.openMark(m)
	}

	r.buf.WriteString(escapeMarkdown(text))

	for _, v := range slices.Backward(marks) {
		if err := r.closeMark(v); err != nil {
			return err
		}
	}

	r.buf.WriteString(trailing)

	return nil
}

// knownMarks drops marks that have no Markdown form, such as Huly's text
// color and highlight marks.
func knownMarks(marks []Mark) []Mark {
	known := make([]Mark, 0, len(marks))

	for _, m := range marks {
		switch m.Type {
		case MarkStrong, MarkEm, MarkUnderline, MarkStrike, MarkCode, MarkLink:
			known = append(known, m)
		}
	}

	return known
}

// maxConsecutiveBackticks returns the length of the longest run of '`' in s.
// Inline code fences must be longer than this value (CommonMark).
func maxConsecutiveBackticks(s string) int {
	longest, cur := 0, 0

	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			longest = max(longest, cur)
		} else {
			cur = 0
		}
	}

	return longest
}

func (r *mdRenderer) renderCodeText(text string, marks []Mark) error {
	fence := strings.Repeat("`", maxConsecutiveBackticks(text)+1)

	var otherMarks []Mark

	for _, m := range marks {
		if m.Type != MarkCode {
			otherMarks = append(otherMarks, m)
		}
	}

	for _, m := range otherMarks {
		r.openMark(m)
	}

	r.buf.WriteString(fence)

	if len(fence) > 1 {
		r.buf.WriteByte(' ')
	}

	r.buf.WriteString(text)

	if len(fence) > 1 {
		r.buf.WriteByte(' ')
	}

	r.buf.WriteString(fence)

	for _, otherMark := range slices.Backward(otherMarks) {
		if err := r.closeMark(otherMark); err != nil {
			return err
		}
	}

	return nil
}

func (r *mdRenderer) openMark(m Mark) {
	switch m.Type {
	case MarkStrong:
		r.buf.WriteString("**")
	case MarkEm:
		r.buf.WriteByte('*')
	case MarkUnderline:
		r.buf.WriteString("<u>")
	case MarkStrike:
		r.buf.WriteString("~~")
	case MarkLink:
		r.buf.WriteByte('[')
	}
}

func (r *mdRenderer) closeMark(m Mark) error {
	switch m.Type {
	case MarkStrong:
		r.buf.WriteString("**")
	case MarkEm:
		r.buf.WriteByte('*')
	case MarkUnderline:
		r.buf.WriteString("</u>")
	case MarkStrike:
		r.buf.WriteString("~~")
	case MarkLink:
		attrs, err := m.linkAttrs()
		if err != nil {
			return fmt.Errorf("cannot render link mark: %w", err)
		}

		r.buf.WriteString("](")
		r.buf.WriteString(safeLinkHref(attrs.Href))

		if attrs.Title != nil {
			r.buf.WriteString(` "`)
			r.buf.WriteString(strings.ReplaceAll(*attrs.Title, `"`, `\"`))
			r.buf.WriteByte('"')
		}

		r.buf.WriteByte(')')
	}

	return nil
}

// checkboxMarker returns the task-list marker for a todo or task item.
func checkboxMarker(item Node) string {
	if item.boolAttr("checked") {
		return "[x] "
	}

	return "[ ] "
}

func (r *mdRenderer) renderBulletList(n Node, marker func(Node) string) error {
	tight, err := listTightness(n)
	if err != nil {
		return fmt.Errorf("cannot render bullet list: %w", err)
	}

	for i, item := range n.Content {
		if i > 0 && !tight {
			r.ensurePrefix()
			r.newLine()
		}

		r.ensurePrefix()
		r.buf.WriteString("- ")

		if marker != nil {
			r.buf.WriteString(marker(item))
		}

		r.atLineStart = false

		oldPrefix := r.prefix
		oldTight := r.tight
		r.prefix += "  "
		r.tight = tight

		if err := r.renderBlocks(item.Content); err != nil {
			r.prefix = oldPrefix
			r.tight = oldTight

			return err
		}

		r.prefix = oldPrefix
		r.tight = oldTight
	}

	return nil
}

func (r *mdRenderer) renderOrderedList(n Node) error {
	first, err := n.orderedListStart()
	if err != nil {
		return fmt.Errorf("cannot render ordered list node: %w", err)
	}

	tight, err := listTightness(n)
	if err != nil {
		return fmt.Errorf("cannot render ordered list: %w", err)
	}

	start := max(first, 1)

	for i, item := range n.Content {
		if i > 0 && !tight {
			r.ensurePrefix()
			r.newLine()
		}

		r.ensurePrefix()

		num := strconv.Itoa(start + i)
		r.buf.WriteString(num)
		r.buf.WriteString(". ")
		r.atLineStart = false

		indent := strings.Repeat(" ", len(num)+2)
		oldPrefix := r.prefix
		oldTight := r.tight
		r.prefix += indent
		r.tight = tight

		if err := r.renderBlocks(item.Content); err != nil {
			r.prefix = oldPrefix
			r.tight = oldTight

			return err
		}

		r.prefix = oldPrefix
		r.tight = oldTight
	}

	return nil
}

func (r *mdRenderer) renderTable(n Node) error {
	if len(n.Content) == 0 {
		return nil
	}

	headerRow := n.Content[0]

	r.ensurePrefix()
	r.buf.WriteByte('|')

	for _, cell := range headerRow.Content {
		r.buf.WriteByte(' ')

		if err := r.renderCellInline(cell); err != nil {
			return err
		}

		r.buf.WriteString(" |")
	}

	r.newLine()

	r.ensurePrefix()
	r.buf.WriteByte('|')

	for range headerRow.Content {
		r.buf.WriteString(" --- |")
	}

	r.newLine()

	for _, row := range n.Content[1:] {
		r.ensurePrefix()
		r.buf.WriteByte('|')

		for _, cell := range row.Content {
			r.buf.WriteByte(' ')

			if err := r.renderCellInline(cell); err != nil {
				return err
			}

			r.buf.WriteString(" |")
		}

		r.newLine()
	}

	return nil
}

func (r *mdRenderer) renderCellInline(cell Node) error {
	if len(cell.Content) == 1 && cell.Content[0].Type == NodeParagraph {
		return r.renderInline(cell.Content[0].Content)
	}

	parts := make([]string, 0, len(cell.Content))
	for _, child := range cell.Content {
		parts = append(parts, PlainText(child))
	}

	r.buf.WriteString(escapeMarkdown(strings.Join(strings.Fields(strings.Join(parts, " ")), " ")))

	return nil
}

// listTightness returns true when the list is tight (no blank lines between items).
// Every direct child of n must be a list item; otherwise listTightness returns an error.
func listTightness(n Node) (tight bool, err error) {
	tight = true

	for _, item := range n.Content {
		switch item.Type {
		case NodeListItem, NodeTodoItem, NodeTaskItem:
		default:
			return false, fmt.Errorf(
				"invalid child type %q (expected %q)",
				item.Type,
				NodeListItem,
			)
		}

		if len(item.Content) != 1 {
			tight = false
		}
	}

	return tight, nil
}

func collectText(nodes []Node) string {
	var buf strings.Builder

	for _, n := range nodes {
		if n.Text != nil {
			buf.WriteString(*n.Text)
		}
	}

	return buf.String()
}

func chooseFence(code string) string {
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`"
	}

	return fence
}

func escapeMarkdown(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))

	for _, c := range s {
		switch c {
		case '\\', '*', '_', '`', '[', ']', '~', '|', '<':
			buf.WriteByte('\\')
		}

		buf.WriteRune(c)
	}

	return buf.String()
}
