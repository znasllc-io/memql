package emaildoc

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// layoutText is the plain-text part of a layout. Each block that has words
// gives one or more chunks, and the chunks are joined by a blank line:
//
//   - a heading and a highlight, as written;
//   - each paragraph of a text block, without its emphasis markers and with
//     each link as "label (address)", and each list, one "- item" or
//     "N. item" line per item;
//   - a button, as "Label: address" (only the label while it has no link);
//   - an html block, the text a reader sees in it.
//
// Pictures, dividers and spacers have no words. The part ends with one
// newline, and is empty when no block has words. Merge tags stay as written,
// to be filled like the rest of the part.
func layoutText(blocks []Block) string {
	var chunks []string
	for _, b := range blocks {
		chunks = appendText(chunks, b)
	}
	if len(chunks) == 0 {
		return ""
	}
	return strings.Join(chunks, "\n\n") + "\n"
}

// appendText adds the chunks of one block.
func appendText(chunks []string, b Block) []string {
	add := func(chunk string) {
		if chunk = strings.Trim(chunk, htmlSpace); chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	switch b.Type {
	case typeHeading, typeHighlight:
		add(oneNewline(b.Text))
	case typeText:
		for _, element := range markdownOf(b.Text) {
			add(elementText(element))
		}
	case typeButton:
		if b.Href == "" {
			add(b.Label)
		} else {
			add(b.Label + ": " + b.Href)
		}
	case typeHTML:
		if normalized, err := safeMarkup(b.HTML); err == nil {
			add(markupText(normalized))
		}
	}
	return chunks
}

// elementText is a paragraph, or a list with an item on each line.
func elementText(element mdElement) string {
	if element.Kind == mdParagraph {
		return inlineText(element.Inline)
	}
	lines := make([]string, len(element.Items))
	for i, item := range element.Items {
		marker := "- "
		if element.Kind == mdNumberedList {
			marker = strconv.Itoa(item.Number) + ". "
		}
		lines[i] = marker + inlineText(item.Inline)
	}
	return strings.Join(lines, "\n")
}

// inlineText is the runs of a paragraph or a list item as plain text.
func inlineText(runs []mdInline) string {
	var b strings.Builder
	for _, run := range runs {
		switch run.Kind {
		case mdLink:
			b.WriteString(run.Text + " (" + run.URL + ")")
		case mdBreak:
			b.WriteByte('\n')
		default:
			b.WriteString(run.Text)
		}
	}
	return b.String()
}

// htmlSpace is what HTML counts as white space. A no-break space is not: it
// stays, as the character it is.
const htmlSpace = " \t\n\f\r"

// lineElements start and end a line of an html block's text.
var lineElements = wordSet("p div table thead tbody tfoot tr caption h1 h2 h3 h4 h5 h6 blockquote pre ul ol li center hr")

// markupText is the text a reader sees in the normalized markup of an html
// block: a line for each block element, list item, table row and line break,
// the cells of a row side by side, white space collapsed as a browser
// collapses it, and blank lines dropped.
func markupText(normalized string) string {
	nodes, err := html.ParseFragment(strings.NewReader(normalized), &html.Node{Type: html.ElementNode, Data: "td", DataAtom: atom.Td})
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
			return
		case n.Type != html.ElementNode:
			return
		case n.Data == "br":
			b.WriteByte('\n')
			return
		case n.Data == "td" || n.Data == "th":
			b.WriteByte(' ')
		case lineElements[n.Data]:
			b.WriteByte('\n')
			defer b.WriteByte('\n')
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, node := range nodes {
		walk(node)
	}
	var lines []string
	for _, line := range strings.Split(b.String(), "\n") {
		if line = strings.Join(strings.FieldsFunc(line, isHTMLSpace), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func isHTMLSpace(r rune) bool { return strings.ContainsRune(htmlSpace, r) }
