package compose

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark/ast"
	tableast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
	"golang.org/x/net/html"
)

type documentBlock struct {
	paragraph string
	table     *documentTable
}

type documentTable struct {
	rows       [][]string // First row is the header.
	alignments []tableast.Alignment
}

// documentBlocks recognizes GFM tables with the same parser as HTML output.
// All other source keeps the existing paragraph rendering. Parsing rather than
// splitting on pipes preserves escaped pipes and leaves fenced examples alone.
func documentBlocks(body string) []documentBlock {
	source := []byte(strings.ReplaceAll(body, "\r\n", "\n"))
	doc := htmlMarkdown.Parser().Parse(text.NewReader(source))
	var blocks []documentBlock
	paragraphs := func(start, end int) {
		for _, paragraph := range splitParagraphs(string(source[start:end])) {
			blocks = append(blocks, documentBlock{paragraph: paragraph})
		}
	}
	previous := 0
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		table, ok := n.(*tableast.Table)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		start := table.FirstChild().Pos()
		end := lineEnd(source, table.LastChild().Pos())
		if table.ChildCount() == 1 {
			// A header-only table still consumes its delimiter row.
			end = lineEnd(source, end)
		}
		paragraphs(previous, start)
		parsed := &documentTable{alignments: table.Alignments}
		for row := table.FirstChild(); row != nil; row = row.NextSibling() {
			cells := make([]string, 0, len(table.Alignments))
			for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
				cells = append(cells, documentCellText(source, cell))
			}
			parsed.rows = append(parsed.rows, cells)
		}
		blocks = append(blocks, documentBlock{table: parsed})
		previous = end
		return ast.WalkSkipChildren, nil
	})
	paragraphs(previous, len(source))
	return blocks
}

func lineEnd(source []byte, start int) int {
	if i := bytes.IndexByte(source[start:], '\n'); i >= 0 {
		return start + i + 1
	}
	return len(source)
}

// Rendering the cell's inline nodes resolves Markdown escapes, entities, links
// and code using Goldmark rather than a second handwritten inline parser.
func documentCellText(source []byte, cell ast.Node) string {
	var rendered bytes.Buffer
	_ = htmlMarkdown.Renderer().Render(&rendered, source, cell)
	z := html.NewTokenizer(bytes.NewReader(htmlBodyPolicy.SanitizeBytes(rendered.Bytes())))
	var plain strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(plain.String())
		case html.TextToken:
			plain.Write(z.Text())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			if string(name) == "br" {
				plain.WriteByte('\n')
			}
		}
	}
}
