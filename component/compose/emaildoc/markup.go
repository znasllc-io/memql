package emaildoc

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The allowlists of an html block. They began as those the Materializer held
// its generated email HTML to, before it wrote emails into layouts, less what
// an html block must not do on its own:
//
//   - no document structure: html, head, body and title belong to the renderer;
//   - no pictures: img and the src attribute are gone. A picture is an image
//     block, whose source is checked and embedded, so nothing in this block can
//     fetch a resource or slip one past the pictures the email declares.
//
// A link's href follows the same rule as every other link of the document.
var (
	markupTags  = wordSet("div span p br hr a table thead tbody tfoot tr td th caption colgroup col h1 h2 h3 h4 h5 h6 strong b em i u s small sup sub blockquote pre code ul ol li center font")
	markupAttrs = wordSet("style class id title lang dir role aria-label aria-hidden align valign bgcolor width height border cellpadding cellspacing colspan rowspan scope face color size target rel href alt")

	// voidTags have no end tag, so they are never open.
	voidTags = wordSet("br hr col")
	// tableParts belong inside a table. Spliced into a table cell, a cell or
	// a row of the block's own would close the renderer's.
	tableParts = wordSet("tr td th thead tbody tfoot caption colgroup col")
)

// A style value that holds any of these can load something or hide something
// that does: backslashes and comments conceal CSS functions, and imports and
// style blocks are not inline styling.
var forbiddenInStyle = []string{`\`, "/*", "@", "url", "image(", "image-set", "expression", "behavior", "-moz-binding"}

// maxMarkupDepth is how many elements an html block may have open at once.
// Email markup nests a few dozen deep at most; the limit keeps a block well
// inside the parser's own (512, implied elements included) and inside a
// browser's, whatever the layout around it adds.
const maxMarkupDepth = 100

// parseFragment is html.ParseFragment. It is a variable only so a test can make
// the parser misbehave and show that safeMarkup does not take its word.
var parseFragment = html.ParseFragment

var (
	errMarkupComments   = errors.New("html blocks cannot contain comments")
	errMarkupCutOff     = errors.New("html blocks cannot end inside a tag; close every tag with >")
	errMarkupUnbalanced = errors.New("html blocks must close the tags they open, in order")
	errMarkupTooDeep    = errors.New("html blocks are nested too deeply")
)

// safeMarkup checks the markup of an html block and returns the form of it
// that goes into the email. The renderer must splice the string this returns,
// never the source: the source is what the author wrote, and a fragment is
// spliced inside a table cell that is the renderer's, in an email that holds a
// footer and an unsubscribe link after it.
//
// The fragment must be self-contained, so a browser reads it as the author
// meant and it cannot reach out of its cell:
//
//   - no comments of any kind, and no doctype: a comment is how markup is shown
//     to one client and hidden from the rest, and an unterminated one hides
//     everything after it;
//   - it does not end inside a tag. The tokenizer drops a tag that the input
//     ends in the middle of, so `<img src="https://evil.example/p.gif"` passes
//     every check on tokens, and spliced before `</td>` it becomes a live tag
//     that swallows the cell's end;
//   - every end tag closes the innermost element the fragment opened, and every
//     element is closed: `</td></tr></table>` cannot close the renderer's cell;
//   - table rows and cells only inside a table the fragment opened;
//   - only the allowed tags and attributes, links by the link rule, and no merge
//     tag in a style (a substituted value would be CSS).
//
// What passes is then read the way a browser reads it, as the content of a
// table cell, and written out again: the result is balanced by construction,
// quotes and angle brackets in text and attributes are escaped, the first of two
// equal attributes is the one that stays, and what browsers repair (an implied
// tbody, a paragraph that a table ends) is repaired. Every element and attribute
// of that result is checked again, so nothing the parser added can slip by.
func safeMarkup(src string) (string, error) {
	if err := checkTokens(src); err != nil {
		return "", err
	}
	nodes, err := parseFragment(strings.NewReader(src), &html.Node{Type: html.ElementNode, Data: "td", DataAtom: atom.Td})
	if err != nil {
		// The only way a string reader fails the parser is by opening more
		// elements than it will hold.
		return "", errMarkupTooDeep
	}
	if err := checkNodes(nodes); err != nil {
		return "", err
	}
	var out strings.Builder
	for _, node := range nodes {
		if err := html.Render(&out, node); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

// checkTokens reads the fragment as the tokenizer does and applies the rules
// about its shape.
func checkTokens(src string) error {
	z := html.NewTokenizer(strings.NewReader(src))
	var (
		open     []string // elements the fragment has opened and not closed
		tables   int      // how many of them are tables
		consumed int      // bytes of src that tokens account for
	)
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if err := z.Err(); err != io.EOF {
				return err
			}
			break
		}
		consumed += len(z.Raw())
		switch kind {
		case html.CommentToken, html.DoctypeToken:
			return errMarkupComments
		case html.StartTagToken, html.SelfClosingTagToken:
			tag := z.Token()
			if err := checkElement(tag.Data, tag.Attr); err != nil {
				return err
			}
			if tableParts[tag.Data] && tables == 0 {
				return fmt.Errorf("html blocks can use <%s> only inside a <table> the block opens", tag.Data)
			}
			if voidTags[tag.Data] {
				continue
			}
			// A slash on any other element is ignored by browsers: <div/> opens.
			if len(open) == maxMarkupDepth {
				return errMarkupTooDeep
			}
			open = append(open, tag.Data)
			if tag.Data == "table" {
				tables++
			}
		case html.EndTagToken:
			name := z.Token().Data
			if err := checkName(name); err != nil {
				return err
			}
			if len(open) == 0 || open[len(open)-1] != name {
				return errMarkupUnbalanced
			}
			open = open[:len(open)-1]
			if name == "table" {
				tables--
			}
		}
	}
	// What is left of src after the last token is a tag cut off by the end of
	// it, which the tokenizer does not report.
	if consumed != len(src) {
		return errMarkupCutOff
	}
	if len(open) > 0 {
		return errMarkupUnbalanced
	}
	return nil
}

// checkName refuses the tags an html block may not use.
func checkName(name string) error {
	if name == "img" {
		return errors.New("html blocks cannot contain pictures; use an image block")
	}
	if !markupTags[name] {
		return fmt.Errorf("html blocks cannot use <%s>", clip(name, 40))
	}
	return nil
}

// checkElement refuses an element that is not allowed, or one of whose
// attributes is not, or whose style loads something, or whose link is not one.
func checkElement(name string, attrs []html.Attribute) error {
	if err := checkName(name); err != nil {
		return err
	}
	for _, attr := range attrs {
		if attr.Namespace != "" || !markupAttrs[attr.Key] || (attr.Key == "href" && name != "a") {
			return fmt.Errorf("html blocks cannot use the %s attribute", clip(attr.Key, 40))
		}
		switch attr.Key {
		case "style":
			// A merge tag is replaced with a value a recipient supplied, and in a
			// style that value would be CSS. Nothing below can see what it will
			// become: u{{fields.a}}rl( turns into url( at send time. So a style
			// holds no merge tag; the other attributes may, as text.
			if strings.Contains(attr.Val, "{{") {
				return errors.New("html block styles cannot contain merge tags")
			}
			value := strings.ToLower(attr.Val)
			for _, forbidden := range forbiddenInStyle {
				if strings.Contains(value, forbidden) {
					return errors.New("html block styles cannot load resources")
				}
			}
		case "href":
			if problem := hrefProblem(attr.Val, true); problem != "" {
				return errors.New(problem)
			}
		}
	}
	return nil
}

// checkNodes applies the allowlists to what the parser built.
func checkNodes(nodes []*html.Node) error {
	for _, node := range nodes {
		if err := checkNode(node); err != nil {
			return err
		}
	}
	return nil
}

func checkNode(n *html.Node) error {
	switch n.Type {
	case html.TextNode:
		return nil
	case html.ElementNode:
		if n.Namespace != "" {
			return fmt.Errorf("html blocks cannot use <%s>", clip(n.Data, 40))
		}
		if err := checkElement(n.Data, n.Attr); err != nil {
			return err
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if err := checkNode(child); err != nil {
				return err
			}
		}
		return nil
	}
	return errMarkupComments
}
