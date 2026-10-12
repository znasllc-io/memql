package emaildoc

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
)

// dumpInline prints inline nodes as t"text", b"..." (bold), i"..." (italic),
// bi"..." (bold italic), a(url)"label" and br, separated by spaces.
func dumpInline(nodes []mdInline) string {
	parts := make([]string, len(nodes))
	for i, n := range nodes {
		switch n.Kind {
		case mdText:
			parts[i] = fmt.Sprintf("t%q", n.Text)
		case mdBold:
			parts[i] = fmt.Sprintf("b%q", n.Text)
		case mdItalic:
			parts[i] = fmt.Sprintf("i%q", n.Text)
		case mdBoldItalic:
			parts[i] = fmt.Sprintf("bi%q", n.Text)
		case mdLink:
			parts[i] = fmt.Sprintf("a(%s)%q", n.URL, n.Text)
		case mdBreak:
			parts[i] = "br"
		default:
			parts[i] = fmt.Sprintf("?%d", n.Kind)
		}
	}
	return strings.Join(parts, " ")
}

// dump prints elements as P(...), UL[...] and OL[N:...], separated by "; ".
func dump(elements []mdElement) string {
	parts := make([]string, len(elements))
	for i, el := range elements {
		switch el.Kind {
		case mdParagraph:
			parts[i] = "P(" + dumpInline(el.Inline) + ")"
		case mdBulletList, mdNumberedList:
			items := make([]string, len(el.Items))
			for j, item := range el.Items {
				items[j] = dumpInline(item.Inline)
				if el.Kind == mdNumberedList {
					items[j] = fmt.Sprintf("%d:%s", item.Number, items[j])
				}
			}
			open, closing := "UL[", "]"
			if el.Kind == mdNumberedList {
				open = "OL["
			}
			parts[i] = open + strings.Join(items, ", ") + closing
		default:
			parts[i] = fmt.Sprintf("?%d", el.Kind)
		}
	}
	return strings.Join(parts, "; ")
}

func TestMarkdownSubset(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// Blocks.
		{"two paragraphs", "one\n\ntwo", `P(t"one"); P(t"two")`},
		{"a single newline is a break", "one\ntwo", `P(t"one" br t"two")`},
		{"bullets", "- a\n- b", `UL[t"a", t"b"]`},
		{"numbers are kept as written", "1. a\n2. b", `OL[1:t"a", 2:t"b"]`},
		{"numbers are not renumbered", "3. a\n1. b\n1. c", `OL[3:t"a", 1:t"b", 1:t"c"]`},
		{"a list needs no blank line", "para\n- item", `P(t"para"); UL[t"item"]`},
		{"a numbered list needs no blank line", "para\n1. item", `P(t"para"); OL[1:t"item"]`},
		{"a paragraph after a list", "- a\ntext", `UL[t"a"]; P(t"text")`},
		{"a blank line ends a list", "- a\n\n- b", `UL[t"a"]; UL[t"b"]`},
		{"list kinds do not mix", "- a\n1. b", `UL[t"a"]; OL[1:t"b"]`},
		{"emphasis in an item", "- **a** b", `UL[b"a" t" b"]`},
		{"ten is a number", "10. ten", `OL[10:t"ten"]`},
		{"ten digits are not a number", "1234567890. x", `P(t"1234567890. x")`},
		{"dash without a space", "-5 degrees\n-- note", `P(t"-5 degrees" br t"-- note")`},
		{"number without a space", "1.5 million", `P(t"1.5 million")`},
		{"a bare dash", "-", `P(t"-")`},
		{"a bare number", "2024.", `P(t"2024.")`},
		{"star is not a bullet", "* item", `P(t"* item")`},
		{"nothing", "", ``},
		{"only blank lines", " \n\n \t\n", ``},
		{"windows line endings", "one\r\n\r\ntwo\r\n- a", `P(t"one"); P(t"two"); UL[t"a"]`},
		{"old mac line endings", "one\rtwo", `P(t"one" br t"two")`},
		{"lines are trimmed", "  one  \n \t two\t", `P(t"one" br t"two")`},
		{"an indented dash is still an item", "   - a", `UL[t"a"]`},
		// Emphasis.
		{"bold italic and both", "**b** *i* ***bi***", `P(b"b" t" " i"i" t" " bi"bi")`},
		{"escaped stars", `\*not italic\*`, `P(t"*not italic*")`},
		{"an unmatched star", "a * b", `P(t"a * b")`},
		{"a star at the start of a line", "* a", `P(t"* a")`},
		{"a star at the end of a line", "a *", `P(t"a *")`},
		{"an opener with no closer", "a *b c", `P(t"a *b c")`},
		{"a closer with no opener", "a b* c", `P(t"a b* c")`},
		{"empty emphasis stays literal", "** **", `P(t"** **")`},
		{"four stars stay literal", "****", `P(t"****")`},
		{"four stars around text stay literal", "****a****", `P(t"****a****")`},
		{"a star inside a word", "a*b*c", `P(t"a" i"b" t"c")`},
		{"italic inside bold", "**a *b* c**", `P(b"a " bi"b" b" c")`},
		{"bold inside italic", "*a **b** c*", `P(i"a " bi"b" i" c")`},
		{"crossing emphasis falls back to literal", "*a **b* c**", `P(i"a **b" t" c**")`},
		{"emphasis across a line break", "**a\nb**", `P(b"a" br b"b")`},
		{"emphasis next to punctuation", `("*a*")`, `P(t"(\"" i"a" t"\")")`},
		{"a star before a no-break space", "*\u00a0a*", fmt.Sprintf("P(t%q)", "*\u00a0a*")},
		{"two separate pairs", "*a* and *b*", `P(i"a" t" and " i"b")`},
		// Links.
		{"a link", "[docs](https://example.com/a?b=1)", `P(a(https://example.com/a?b=1)"docs")`},
		{"a link between words", "see [docs](https://example.com) now", `P(t"see " a(https://example.com)"docs" t" now")`},
		{"a link tag as the destination", "[unsubscribe]({{unsubscribeUrl}})", `P(a({{unsubscribeUrl}})"unsubscribe")`},
		{"parentheses that balance", "[w](https://en.wikipedia.org/wiki/Foo_(bar))", `P(a(https://en.wikipedia.org/wiki/Foo_(bar))"w")`},
		{"an escaped parenthesis", `[w](https://x.com/a\)b)`, `P(a(https://x.com/a)b)"w")`},
		{"an escaped bracket in the label", `[a\]b](https://x.com)`, `P(a(https://x.com)"a]b")`},
		{"escaped brackets are text", `\[not a link\](https://example.com)`, `P(t"[not a link](https://example.com)")`},
		{"an unclosed destination", "[docs](https://example.com", `P(t"[docs](https://example.com")`},
		{"a label with no destination", "[docs] and (more)", `P(t"[docs] and (more)")`},
		{"a label left open", "[docs and more", `P(t"[docs and more")`},
		{"an empty destination is still a link", "[x]()", `P(a()"x")`},
		{"a link inside bold", "**see [docs](https://example.com)**", `P(b"see " a(https://example.com)"docs")`},
		{"the label is plain text", "[**docs**](https://example.com)", `P(a(https://example.com)"**docs**")`},
		{"an image is a bang and a link", "![alt](https://example.com/a.png)", `P(t"!" a(https://example.com/a.png)"alt")`},
		{"the inner bracket wins", "[a [b](https://x.com)", `P(t"[a " a(https://x.com)"b")`},
		{"a label across lines", "[two\nlines](https://x.com)", `P(a(https://x.com)"two lines")`},
		{"links in items", "- [a](https://a.com)\n- [b](https://b.com)", `UL[a(https://a.com)"a", a(https://b.com)"b"]`},
		// Everything else is literal.
		{"a merge tag", "{{displayName}}", `P(t"{{displayName}}")`},
		{"field merge tags", "Hi {{fields.first_name}}, {{accountName}}", `P(t"Hi {{fields.first_name}}, {{accountName}}")`},
		{"html is text", "<b>x</b> & y", `P(t"<b>x</b> & y")`},
		{"headings are text", "# Title", `P(t"# Title")`},
		{"underscores are text", "_a_ and __b__", `P(t"_a_ and __b__")`},
		{"backticks are text", "`code`", "P(t\"`code`\")"},
		{"a backslash before a letter", `a\b`, `P(t"a\\b")`},
		{"a trailing backslash", `a\`, `P(t"a\\")`},
		{"an escaped backslash", `a\\b`, `P(t"a\\b")`},
		{"escaped parentheses", `\(a\)`, `P(t"(a)")`},
		{"non-ASCII text", "café **naïve** 日本", "P(t\"café \" b\"naïve\" t\" 日本\")"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseMarkdown(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if d := dump(got); d != c.want {
				t.Fatalf("%q\n got: %s\nwant: %s", c.in, d, c.want)
			}
		})
	}
}

func TestMarkdownRefusesALinkWithoutALabel(t *testing.T) {
	for _, in := range []string{"[](https://example.com)", "[ ](https://example.com)", "text [\t](https://example.com) text", "- [](https://example.com)"} {
		if _, err := parseMarkdown(in); err == nil || !strings.Contains(err.Error(), "a link needs a label") {
			t.Errorf("%q: got %v", in, err)
		}
	}
}

// within fails the test when f takes longer than limit. Every input below runs
// in a few milliseconds when the parser is linear and for minutes when it is
// quadratic, so the margin is wide enough not to flake on a slow machine.
func within(t *testing.T, limit time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("took longer than %v", limit)
	}
}

func TestMarkdownIsLinear(t *testing.T) {
	const size = 1 << 20
	repeat := func(unit string) string { return strings.Repeat(unit, size/len(unit)) }
	for name, in := range map[string]string{
		"openers that never close":      repeat("*a "),
		"closers of the wrong length":   repeat("*a ") + repeat("a** "),
		"bold openers then italic":      repeat("**a ") + repeat("a* "),
		"alternating stars":             repeat("a*"),
		"only stars":                    repeat("*"),
		"links that never close":        repeat("[a]("),
		"labels that never close":       repeat("["),
		"closing brackets":              repeat("]"),
		"open parentheses":              repeat("("),
		"nested parentheses":            repeat("(") + repeat(")"),
		"a link around a long tail":     "[a](" + repeat("(") + ")",
		"unmatched brackets and stars":  repeat("[*a]( "),
		"backslashes":                   repeat(`\`),
		"escaped stars":                 repeat(`\*`),
		"many list items":               repeat("- a\n"),
		"many numbered items":           repeat("1. a\n"),
		"one paragraph of many lines":   repeat("a\n"),
		"many paragraphs":               repeat("a\n\n"),
		"a very long line of text":      repeat("a"),
		"emphasis around many breaks":   "**" + repeat("a\n") + "**",
		"links and stars interleaved":   repeat("*[a](b)"),
		"crossing pairs of every kind":  repeat("*a **b *** c* d** "),
		"stars and spaces":              repeat("* "),
		"lists with unmatched emphasis": repeat("- *a\n"),
	} {
		t.Run(name, func(t *testing.T) {
			within(t, 20*time.Second, func() { _, _ = parseMarkdown(in) })
		})
	}
}

// checkMarkdownInvariants states what the renderer relies on.
func checkMarkdownInvariants(t *testing.T, in string, elements []mdElement) {
	t.Helper()
	check := func(nodes []mdInline, where string) {
		if len(nodes) == 0 {
			t.Fatalf("%q: %s is empty", in, where)
		}
		if nodes[0].Kind == mdBreak || nodes[len(nodes)-1].Kind == mdBreak {
			t.Fatalf("%q: %s starts or ends with a break", in, where)
		}
		for i, n := range nodes {
			switch n.Kind {
			case mdText, mdBold, mdItalic, mdBoldItalic:
				if n.Text == "" {
					t.Fatalf("%q: %s has an empty text node", in, where)
				}
				if strings.Contains(n.Text, "\n") {
					t.Fatalf("%q: %s has a line break inside text %q", in, where, n.Text)
				}
				if i > 0 && nodes[i-1].Kind == n.Kind {
					t.Fatalf("%q: %s has two adjacent nodes of kind %d", in, where, n.Kind)
				}
			case mdLink:
				if strings.TrimFunc(n.Text, unicode.IsSpace) == "" {
					t.Fatalf("%q: %s has a link without a label", in, where)
				}
			case mdBreak:
				if i > 0 && nodes[i-1].Kind == mdBreak {
					t.Fatalf("%q: %s has two breaks in a row", in, where)
				}
			default:
				t.Fatalf("%q: %s has a node of kind %d", in, where, n.Kind)
			}
		}
	}
	for i, el := range elements {
		switch el.Kind {
		case mdParagraph:
			check(el.Inline, fmt.Sprintf("paragraph %d", i))
		case mdBulletList, mdNumberedList:
			if len(el.Items) == 0 {
				t.Fatalf("%q: list %d has no items", in, i)
			}
			for j, item := range el.Items {
				check(item.Inline, fmt.Sprintf("list %d item %d", i, j))
				for _, n := range item.Inline {
					if n.Kind == mdBreak {
						t.Fatalf("%q: a list item spans lines", in)
					}
				}
			}
		default:
			t.Fatalf("%q: element %d has kind %d", in, i, el.Kind)
		}
	}
}

func FuzzParseMarkdown(f *testing.F) {
	for _, seed := range []string{
		"one\n\ntwo",
		"- a\n- b\n1. c\n2. d",
		"**b** *i* ***bi*** \\*x\\* a * b",
		"[docs](https://example.com/a?b=1) [w](https://x.com/a_(b)) [](x) [a [b](c)",
		"para\n- item\n10. ten\n1234567890. no",
		"*a **b* c** \\ \\\\ \\( \\) \\[ \\] ![i](u)",
		"{{displayName}} <b>&</b>",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		elements, err := parseMarkdown(in)
		if err != nil {
			if !strings.Contains(err.Error(), "a link needs a label") {
				t.Fatalf("%q: unexpected error %v", in, err)
			}
			return
		}
		checkMarkdownInvariants(t, in, elements)
	})
}
