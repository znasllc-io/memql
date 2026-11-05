package emaildoc

import (
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

var wireframeColours = map[string]bool{
	"#ffffff": true, "#ececec": true, "#d6d6d6": true,
	"#e2e2e2": true, "#c8c8c8": true, "#bdbdbd": true,
}

func TestWireframeUsesOnlyGreysAndShapes(t *testing.T) {
	allowed := map[string]bool{"svg": true, "rect": true, "circle": true, "line": true, "g": true}
	colour := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	for _, info := range Layouts() {
		svg := Wireframe(info.Document)
		if !strings.HasPrefix(svg, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 `) {
			t.Fatalf("%s: unexpected start %.80q", info.ID, svg)
		}
		root, err := html.Parse(strings.NewReader(svg))
		if err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		var walk func(n *html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode {
				switch n.Data {
				case "html", "head", "body":
				default:
					count[n.Data]++
					if !allowed[n.Data] {
						t.Errorf("%s: element <%s> is not allowed", info.ID, n.Data)
					}
				}
				stroked, nonScaling := false, false
				for _, a := range n.Attr {
					if a.Key == "stroke" {
						stroked = true
					}
					if a.Key == "vector-effect" && a.Val == "non-scaling-stroke" {
						nonScaling = true
					}
				}
				if stroked && !nonScaling {
					t.Errorf("%s: a stroked <%s> lacks vector-effect non-scaling-stroke", info.ID, n.Data)
				}
				for _, a := range n.Attr {
					for _, c := range colour.FindAllString(a.Val, -1) {
						if !wireframeColours[strings.ToLower(c)] {
							t.Errorf("%s: colour %s is not allowed", info.ID, c)
						}
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(root)
		if count["svg"] != 1 || count["rect"] < 3 {
			t.Errorf("%s: implausible drawing %v", info.ID, count)
		}
		if strings.Contains(svg, "<text") || strings.Contains(svg, "<image") {
			t.Errorf("%s: text or image element", info.ID)
		}
	}
}

func TestWireframeIsDeterministic(t *testing.T) {
	for _, info := range Layouts() {
		a, b := Wireframe(info.Document), Wireframe(info.Document)
		if a != b {
			t.Errorf("%s: two calls differ", info.ID)
		}
	}
}

func TestLayoutsWireframeGolden(t *testing.T) {
	for _, info := range Layouts() {
		name := strings.TrimPrefix(info.ID, "builtin:")
		t.Run(name, func(t *testing.T) {
			doc, err := ExpandToMinimum(info.Document, info)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "wireframe-"+name+".svg", Wireframe(doc))
		})
	}
}
