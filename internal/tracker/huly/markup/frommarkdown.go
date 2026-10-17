package markup

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	headingLine = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	bulletLine  = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	orderedLine = regexp.MustCompile(`^\s*(\d+)[.)]\s+(.*)$`)
	fenceLine   = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([^`\\s]*)\\s*$")
)

// FromMarkdown builds a serialized markup document from the Markdown that
// Sortie writes into issue bodies: ATX headings, bullet and ordered lists,
// fenced code blocks, and paragraphs, with inline code spans. Other
// Markdown syntax is kept as literal text, so nothing is lost.
func FromMarkdown(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var blocks []Node

	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "":
			i++

		case fenceLine.MatchString(line):
			m := fenceLine.FindStringSubmatch(line)
			fence := m[1]
			var body []string
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) != fence {
				body = append(body, lines[i])
				i++
			}
			i++ // the closing fence, or past the end
			block := Node{Type: NodeCodeBlock}
			if text := strings.Join(body, "\n"); text != "" {
				block.Content = []Node{textNode(text)}
			}
			if m[2] != "" {
				block.Attrs = mustAttrs(map[string]any{"language": m[2]})
			}
			blocks = append(blocks, block)

		case headingLine.MatchString(line):
			m := headingLine.FindStringSubmatch(line)
			blocks = append(blocks, Node{
				Type:    NodeHeading,
				Attrs:   mustAttrs(map[string]any{"level": len(m[1])}),
				Content: inline(m[2]),
			})
			i++

		case bulletLine.MatchString(line):
			list := Node{Type: NodeBulletList}
			for i < len(lines) && bulletLine.MatchString(lines[i]) {
				list.Content = append(list.Content, listItem(bulletLine.FindStringSubmatch(lines[i])[1]))
				i++
			}
			blocks = append(blocks, list)

		case orderedLine.MatchString(line):
			start, _ := strconv.Atoi(orderedLine.FindStringSubmatch(line)[1])
			list := Node{Type: NodeOrderedList, Attrs: mustAttrs(map[string]any{"start": start})}
			for i < len(lines) && orderedLine.MatchString(lines[i]) {
				list.Content = append(list.Content, listItem(orderedLine.FindStringSubmatch(lines[i])[2]))
				i++
			}
			blocks = append(blocks, list)

		default:
			var para []string
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" && !startsBlock(lines[i]) {
				para = append(para, strings.TrimSpace(lines[i]))
				i++
			}
			blocks = append(blocks, Node{Type: NodeParagraph, Content: inline(strings.Join(para, " "))})
		}
	}

	return serialize(doc(blocks...))
}

func startsBlock(line string) bool {
	return fenceLine.MatchString(line) || headingLine.MatchString(line) ||
		bulletLine.MatchString(line) || orderedLine.MatchString(line)
}

func listItem(text string) Node {
	return Node{Type: NodeListItem, Content: []Node{{Type: NodeParagraph, Content: inline(text)}}}
}

// inline splits text into plain text and code spans.
func inline(text string) []Node {
	var out []Node
	for text != "" {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start+1:], '`')
		if end < 0 {
			break
		}
		if start > 0 {
			out = append(out, textNode(text[:start]))
		}
		if code := text[start+1 : start+1+end]; code != "" {
			n := textNode(code)
			n.Marks = []Mark{{Type: MarkCode}}
			out = append(out, n)
		}
		text = text[start+2+end:]
	}
	if text != "" {
		out = append(out, textNode(text))
	}
	return out
}

func mustAttrs(v map[string]any) json.RawMessage {
	// Marshaling a map of strings and ints cannot fail.
	raw, _ := json.Marshal(v)
	return raw
}
