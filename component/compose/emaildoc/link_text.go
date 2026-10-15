package emaildoc

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// LinkTextProblem checks recipient-link tags in visible, plain text. Mail
// clients turn addresses in text into links, so a signed recipient link must
// never become a query parameter or another part of somebody else's address.
// Callers with markup must first decode its visible text and check link
// destinations separately. A period ending a sentence remains ordinary prose.
func LinkTextProblem(text string) string {
	for _, tag := range linkTags {
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
				attached = linkAddressRune(r)
			}
			if end < len(text) {
				r, size := utf8.DecodeRuneInString(text[end:])
				terminalPeriod := r == '.' && (end+size == len(text) || !linkAddressRuneAt(text[end+size:]))
				attached = attached || (linkAddressRune(r) && !terminalPeriod)
			}
			if attached {
				return tag + " must be the whole link, never part of another address"
			}
			offset = end
		}
	}
	return ""
}

func linkAddressRuneAt(text string) bool {
	r, _ := utf8.DecodeRuneInString(text)
	return linkAddressRune(r)
}

func linkAddressRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || strings.ContainsRune("=&?/#%+-._~:@", r)
}

// markdownLinkFacts uses the same parsed runs as the renderer. Link labels
// cannot stand in for links when a generated draft preserves a layout's tags.
func markdownLinkFacts(text string) (mentions, links map[string]int, problem string) {
	elements, err := parseMarkdown(text)
	if err != nil {
		return nil, nil, err.Error()
	}
	mentions, links = map[string]int{}, map[string]int{}
	inspect := func(runs []mdInline) string {
		var visible strings.Builder
		for _, run := range runs {
			if run.Kind == mdLink {
				if problem := hrefProblem(run.URL, false); problem != "" {
					return problem
				}
				for _, tag := range linkTags {
					if strings.Contains(run.Text, tag) {
						return "A recipient link belongs in the link address, not its label."
					}
				}
				if tag := linkTagOf(run.URL); tag != "" {
					links[tag]++
					mentions[tag]++
				}
			}
			if run.Kind == mdBreak {
				visible.WriteByte('\n')
			} else {
				visible.WriteString(run.Text)
			}
		}
		plain := visible.String()
		if problem := LinkTextProblem(plain); problem != "" {
			return problem
		}
		for _, tag := range linkTags {
			mentions[tag] += strings.Count(plain, tag)
		}
		return ""
	}
	for _, element := range elements {
		if problem := inspect(element.Inline); problem != "" {
			return nil, nil, problem
		}
		for _, item := range element.Items {
			if problem := inspect(item.Inline); problem != "" {
				return nil, nil, problem
			}
		}
	}
	return mentions, links, ""
}
