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
// Derived from go.probo.inc/probo/pkg/prosemirror (node.go at commit
// a89724c2d519732aa13caa72331f03d8b3f80976). Modified for Sortie: trimmed to
// the attributes the markdown renderer reads, added the Huly node and mark
// types, and accepted Huly's "order" attribute on ordered lists.

package markup

import (
	"encoding/json"
	"fmt"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

type (
	// NodeType identifies the type of a ProseMirror/Tiptap document node.
	NodeType string

	// MarkType identifies the type of an inline mark.
	MarkType string

	// Node is a ProseMirror/Tiptap document node.
	Node struct {
		Type    NodeType        `json:"type"`
		Content []Node          `json:"content,omitempty"`
		Marks   []Mark          `json:"marks,omitempty"`
		Text    *string         `json:"text,omitempty"`
		Attrs   json.RawMessage `json:"attrs,omitempty"`
	}

	// Mark represents inline formatting applied to a text node.
	Mark struct {
		Type  MarkType        `json:"type"`
		Attrs json.RawMessage `json:"attrs,omitempty"`
	}

	headingAttrs struct {
		Level int `json:"level"`
	}

	codeBlockAttrs struct {
		Language *string `json:"language"`
	}

	orderedListAttrs struct {
		Start *int `json:"start"`
		Order *int `json:"order"`
	}

	imageAttrs struct {
		Src   string  `json:"src"`
		Alt   *string `json:"alt"`
		Title *string `json:"title"`
	}

	linkAttrs struct {
		Href  string  `json:"href"`
		Title *string `json:"title"`
	}
)

// Node and mark type names. String values match the Tiptap JSON that Huly
// stores.
const (
	NodeDoc            NodeType = "doc"
	NodeParagraph      NodeType = "paragraph"
	NodeBlockquote     NodeType = "blockquote"
	NodeHeading        NodeType = "heading"
	NodeCodeBlock      NodeType = "codeBlock"
	NodeHorizontalRule NodeType = "horizontalRule"
	NodeHardBreak      NodeType = "hardBreak"
	NodeText           NodeType = "text"
	NodeImage          NodeType = "image"
	NodeBulletList     NodeType = "bulletList"
	NodeOrderedList    NodeType = "orderedList"
	NodeListItem       NodeType = "listItem"
	NodeTable          NodeType = "table"
	NodeTableRow       NodeType = "tableRow"
	NodeTableCell      NodeType = "tableCell"
	NodeTableHeader    NodeType = "tableHeader"

	// Huly-specific nodes.
	NodeReference NodeType = "reference"
	NodeEmoji     NodeType = "emoji"
	NodeMermaid   NodeType = "mermaid"
	NodeTodoList  NodeType = "todoList"
	NodeTodoItem  NodeType = "todoItem"
	NodeTaskList  NodeType = "taskList"
	NodeTaskItem  NodeType = "taskItem"

	MarkStrong    MarkType = "bold"
	MarkEm        MarkType = "italic"
	MarkUnderline MarkType = "underline"
	MarkStrike    MarkType = "strike"
	MarkCode      MarkType = "code"
	MarkLink      MarkType = "link"
)

// Parse unmarshals a ProseMirror/Tiptap JSON document string into a Node tree.
func Parse(s string) (Node, error) {
	var n Node
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		return Node{}, fmt.Errorf("cannot parse prosemirror node: %w", err)
	}

	return n, nil
}

func (n Node) headingAttrs() (headingAttrs, error) {
	a := headingAttrs{Level: 1}
	if len(n.Attrs) == 0 {
		return a, nil
	}

	if err := json.Unmarshal(n.Attrs, &a); err != nil {
		return a, fmt.Errorf("cannot parse heading attrs: %w", err)
	}

	return a, nil
}

func (n Node) codeBlockAttrs() (codeBlockAttrs, error) {
	if len(n.Attrs) == 0 {
		return codeBlockAttrs{}, nil
	}

	var a codeBlockAttrs
	if err := json.Unmarshal(n.Attrs, &a); err != nil {
		return a, fmt.Errorf("cannot parse code block attrs: %w", err)
	}

	return a, nil
}

// orderedListStart returns the first item number. Huly stores it as
// "order"; stock Tiptap as "start".
func (n Node) orderedListStart() (int, error) {
	if len(n.Attrs) == 0 {
		return 1, nil
	}

	var a orderedListAttrs
	if err := json.Unmarshal(n.Attrs, &a); err != nil {
		return 1, fmt.Errorf("cannot parse ordered list attrs: %w", err)
	}

	switch {
	case a.Order != nil:
		return *a.Order, nil
	case a.Start != nil:
		return *a.Start, nil
	default:
		return 1, nil
	}
}

func (n Node) imageAttrs() (imageAttrs, error) {
	var a imageAttrs
	if len(n.Attrs) == 0 {
		return a, nil
	}

	if err := json.Unmarshal(n.Attrs, &a); err != nil {
		return a, fmt.Errorf("cannot parse image attrs: %w", err)
	}

	return a, nil
}

// stringAttr returns the named attribute when it holds a JSON string, and
// the empty string otherwise.
func (n Node) stringAttr(name string) string {
	if len(n.Attrs) == 0 {
		return ""
	}

	var attrs map[string]any
	if err := json.Unmarshal(n.Attrs, &attrs); err != nil {
		return ""
	}

	s, fault := typeutil.StringField(attrs, name)
	if fault != nil {
		return ""
	}

	return s
}

// boolAttr returns the named attribute when it holds a JSON boolean, and
// false otherwise.
func (n Node) boolAttr(name string) bool {
	if len(n.Attrs) == 0 {
		return false
	}

	var attrs map[string]any
	if err := json.Unmarshal(n.Attrs, &attrs); err != nil {
		return false
	}

	b, _ := attrs[name].(bool)

	return b
}

func (m Mark) linkAttrs() (linkAttrs, error) {
	var a linkAttrs
	if err := json.Unmarshal(m.Attrs, &a); err != nil {
		return a, fmt.Errorf("cannot parse link attrs: %w", err)
	}

	return a, nil
}
