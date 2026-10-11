package emaildoc

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// The allowlists of an html block. They are those of the Materializer's
// generated email markup (integrations/compose, validateGeneratedEmailMarkup),
// less what an html block must not do on its own:
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
)

// A style value that holds any of these can load something or hide something
// that does: backslashes and comments conceal CSS functions, and imports and
// style blocks are not inline styling.
var forbiddenInStyle = []string{`\`, "/*", "@", "url", "image(", "image-set", "expression", "behavior", "-moz-binding"}

// validateMarkup refuses an html block that uses markup outside the allowlist.
// The block is validated, never silently cleaned: what the author wrote is
// what is sent, or the document is not accepted. The sentences it returns
// start with "html blocks cannot" (or, for a link, "a link ...") and Parse
// puts the block's name in front.
func validateMarkup(src string) error {
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); err != io.EOF {
				return err
			}
			return nil
		case html.CommentToken:
			if isConditionalComment(z.Token().Data) {
				return errors.New("html blocks cannot contain conditional comments")
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			if err := checkTag(z.Token()); err != nil {
				return err
			}
		}
	}
}

// isConditionalComment spots the Outlook conditionals. "<!--[if mso]>...
// <![endif]-->" keeps markup inside a comment, which is how it shows tags to
// one client and hides them from every other; the tokenizer reads the revealed
// forms "<![if !mso]>" and "<![endif]>" as comments that begin with the
// condition.
func isConditionalComment(data string) bool {
	if strings.Contains(data, "<") {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(data))
	return strings.HasPrefix(lower, "[if") || strings.HasPrefix(lower, "[endif")
}

func checkTag(tag html.Token) error {
	if tag.Data == "img" {
		return errors.New("html blocks cannot contain pictures; use an image block")
	}
	if !markupTags[tag.Data] {
		return fmt.Errorf("html blocks cannot use <%s>", clip(tag.Data, 40))
	}
	for _, attr := range tag.Attr {
		if !markupAttrs[attr.Key] || (attr.Key == "href" && tag.Data != "a") {
			return fmt.Errorf("html blocks cannot use the %s attribute", clip(attr.Key, 40))
		}
		switch attr.Key {
		case "style":
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
