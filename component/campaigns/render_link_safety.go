package campaigns

import (
	"fmt"
	nethtml "golang.org/x/net/html"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

var recipientLinkTags = []string{"{{unsubscribeUrl}}", "{{preferencesUrl}}", "{{viewUrl}}", "{{confirmUrl}}"}

func carriesRecipientLinkTag(value string) bool {
	for _, tag := range recipientLinkTags {
		if strings.Contains(value, tag) {
			return true
		}
	}
	return false
}
func recipientLinkTextProblem(text string) string {
	addressRune := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || strings.ContainsRune("=&?/#%+-._~:@", r)
	}
	for _, tag := range recipientLinkTags {
		for offset := 0; offset < len(text); {
			at := strings.Index(text[offset:], tag)
			if at < 0 {
				break
			}
			at += offset
			end := at + len(tag)
			attached := false
			if at > 0 {
				r, _ := utf8.DecodeLastRuneInString(text[:at])
				attached = addressRune(r)
			}
			if end < len(text) {
				r, size := utf8.DecodeRuneInString(text[end:])
				next, _ := utf8.DecodeRuneInString(text[end+size:])
				terminalPeriod := r == '.' && (end+size == len(text) || !addressRune(next))
				attached = attached || (addressRune(r) && !terminalPeriod)
			}
			if attached {
				return tag + " must be the whole link, never part of another address"
			}
			offset = end
		}
	}
	return ""
}

// Validate before mint substitutions reach any byte of authored markup. Parsing
// is read-only: the byte-preserving renderer still sends the original template.
// Inspect both attributes and decoded visible text, including text split by inline
// elements; otherwise a signed URL can become a query parameter at another host.
func validateRecipientLinkLocations(t Template) error {
	if carriesRecipientLinkTag(t.Subject) {
		return fmt.Errorf("recipient links belong in the message body, not its subject")
	}
	if problem := recipientLinkTextProblem(t.TextBody); problem != "" {
		return fmt.Errorf("%s", problem)
	}
	if t.HTMLBody == "" {
		return nil
	}
	// A token in a comment or an unfinished tag must never be substituted either.
	tokenizer := nethtml.NewTokenizer(strings.NewReader(t.HTMLBody))
	for {
		typ := tokenizer.Next()
		if typ == nethtml.ErrorToken {
			if err := tokenizer.Err(); err != io.EOF {
				return err
			}
			break
		}
		if typ == nethtml.CommentToken && carriesRecipientLinkTag(string(tokenizer.Raw())) {
			raw := strings.TrimSpace(string(tokenizer.Text()))
			lower := strings.ToLower(raw)
			end := strings.Index(lower, "]>")
			if !strings.HasPrefix(lower, "[if ") || end < 0 || !strings.HasSuffix(lower, "<![endif]") {
				return fmt.Errorf("recipient links cannot appear in HTML comments")
			}
			if err := validateRecipientLinkLocations(Template{HTMLBody: raw[end+2 : len(raw)-len("<![endif]")]}); err != nil {
				return err
			}
		}
	}
	doc, err := nethtml.ParseWithOptions(strings.NewReader(t.HTMLBody), nethtml.ParseOptionEnableScripting(false))
	if err != nil {
		return err
	}
	var visible strings.Builder
	var walk func(*nethtml.Node, bool, bool) error
	walk = func(n *nethtml.Node, inAnchor, hidden bool) error {
		block := false
		if n.Type == nethtml.ElementNode {
			switch n.Data {
			case "script", "style", "template", "title", "textarea", "xmp", "iframe", "noembed", "noframes":
				hidden = true
			}
			for _, a := range n.Attr {
				if !carriesRecipientLinkTag(a.Val) {
					continue
				}
				allowed := false
				for _, tag := range recipientLinkTags {
					if n.Namespace == "" && n.Data == "a" && a.Key == "href" && strings.TrimSpace(a.Val) == tag {
						allowed = true
					}
				}
				if !allowed {
					return fmt.Errorf("a recipient link must be the whole href of an email link")
				}
			}
			inAnchor = inAnchor || n.Data == "a"
			switch n.Data {
			case "p", "div", "br", "li", "tr", "td", "h1", "h2", "h3", "section", "table":
				block = true
				visible.WriteByte('\n')
			}
		}
		if n.Type == nethtml.TextNode {
			if carriesRecipientLinkTag(n.Data) && (hidden || inAnchor) {
				return fmt.Errorf("recipient links belong in a link address or visible standalone text")
			}
			if !hidden {
				visible.WriteString(n.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c, inAnchor, hidden); err != nil {
				return err
			}
		}
		if block {
			visible.WriteByte('\n')
		}
		return nil
	}
	if err = walk(doc, false, false); err != nil {
		return err
	}
	if problem := recipientLinkTextProblem(visible.String()); problem != "" {
		return fmt.Errorf("%s", problem)
	}
	return nil
}
