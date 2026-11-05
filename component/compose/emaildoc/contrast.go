package emaildoc

import (
	"math"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// luminanceOf is the WCAG 2.x relative luminance of a "#rrggbb" colour.
func luminanceOf(hex string) float64 {
	channel := func(from int) float64 {
		v, _ := strconv.ParseUint(hex[from:from+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(1) + 0.7152*channel(3) + 0.0722*channel(5)
}

// ContrastRatio is the WCAG 2.x contrast ratio of two "#rrggbb" colours: the
// luminance of the lighter plus 0.05, over that of the darker plus 0.05.
func ContrastRatio(a, b string) float64 {
	la, lb := luminanceOf(a), luminanceOf(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TextRun is some words of an email as a reader sees them: their colour, the
// colour behind them, their size, whether they are bold, and the block they
// belong to (a preview render marks each block's outer cell).
type TextRun struct {
	Block      string
	Words      string
	Foreground string
	Background string
	Size       float64
	Bold       bool
}

// Large says whether WCAG treats the words as large text: 24 pixels, or 18.66
// pixels (14 points) and bold.
func (r TextRun) Large() bool { return r.Size >= 24 || (r.Bold && r.Size >= 18.66) }

// Required is the contrast the words need: 4.5 to 1, or 3 to 1 for large text.
func (r TextRun) Required() float64 {
	if r.Large() {
		return 3
	}
	return 4.5
}

// hexColour reads "#rgb" or "#rrggbb" as "#rrggbb"; anything else (a keyword,
// a function) is not a colour the renderer writes for text.
func hexColour(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if len(v) == 4 && v[0] == '#' {
		return "#" + strings.Repeat(v[1:2], 2) + strings.Repeat(v[2:3], 2) + strings.Repeat(v[3:4], 2), true
	}
	if len(v) == 7 && v[0] == '#' {
		return v, true
	}
	return "", false
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// styleOf reads an element's style attribute into property -> value.
func styleOf(n *html.Node) map[string]string {
	decl := map[string]string{}
	for _, part := range strings.Split(attrOf(n, "style"), ";") {
		if name, value, ok := strings.Cut(part, ":"); ok {
			decl[strings.ToLower(strings.TrimSpace(name))] = strings.ToLower(strings.TrimSpace(value))
		}
	}
	return decl
}

// TextRuns reads the words of an email the way a reader meets them. Colour,
// size and weight are inherited, as in a browser, starting from black on white
// at 16 pixels; the colour behind words is the nearest background of an
// element around them (a page, the canvas, a container, the cell of a button,
// the span of a highlight). What is not shown (the head, comments, the hidden
// preheader) is left out, and so are white space and the empty cells that hold
// only a no-break space.
func TextRuns(markup string) ([]TextRun, error) {
	page, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return nil, err
	}
	type state struct {
		fg, bg string
		size   float64
		bold   bool
		block  string
	}
	var runs []TextRun
	var walk func(n *html.Node, st state)
	walk = func(n *html.Node, st state) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				if words := strings.TrimSpace(c.Data); words != "" && st.size >= 1 {
					runs = append(runs, TextRun{Block: st.block, Words: words, Foreground: st.fg, Background: st.bg, Size: st.size, Bold: st.bold})
				}
			case html.ElementNode:
				switch c.Data {
				case "head", "style", "script", "title", "noscript":
					continue
				}
				decl := styleOf(c)
				if strings.Contains(decl["display"], "none") {
					continue
				}
				next := st
				if v, ok := hexColour(decl["color"]); ok {
					next.fg = v
				}
				if v, ok := hexColour(decl["background-color"]); ok {
					next.bg = v
				} else if v, ok := hexColour(attrOf(c, "bgcolor")); ok {
					next.bg = v
				}
				if v, err := strconv.ParseFloat(strings.TrimSuffix(decl["font-size"], "px"), 64); err == nil && decl["font-size"] != "" {
					next.size = v
				}
				switch decl["font-weight"] {
				case "700", "bold":
					next.bold = true
				case "400", "normal":
					next.bold = false
				}
				if id := attrOf(c, "data-block"); id != "" {
					next.block = id
				}
				walk(c, next)
			}
		}
	}
	walk(page, state{fg: "#000000", bg: "#ffffff", size: 16})
	return runs, nil
}
