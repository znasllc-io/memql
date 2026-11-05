package emaildoc

import (
	"errors"
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

// TestLayoutsWireframeGolden pins each built-in layout's picker drawing,
// BuiltinWireframe, to its golden.
func TestLayoutsWireframeGolden(t *testing.T) {
	for _, info := range Layouts() {
		name := strings.TrimPrefix(info.ID, "builtin:")
		t.Run(name, func(t *testing.T) {
			svg, err := BuiltinWireframe(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "wireframe-"+name+".svg", svg)
		})
	}
}

// TestBuiltinWireframeDrawsTheMinimumForm: a picker shows a layout in its
// smallest form, so Steps, whose steps start at two, is drawn with two; and an
// id that names no built-in layout is refused as one.
func TestBuiltinWireframeDrawsTheMinimumForm(t *testing.T) {
	info, _ := LayoutMeta("builtin:steps")
	doc, _ := Layout("builtin:steps")
	expanded, err := ExpandToMinimum(doc, info)
	if err != nil {
		t.Fatal(err)
	}
	if Wireframe(expanded) == Wireframe(doc) {
		t.Fatal("the minimum form of steps draws like the bare layout; the test proves nothing")
	}
	got, err := BuiltinWireframe("builtin:steps")
	if err != nil || got != Wireframe(expanded) {
		t.Fatalf("steps drawn as %.60q (%v), want its minimum form", got, err)
	}
	for _, id := range []string{"builtin:nope", "steps", "org:abc123", ""} {
		if _, err := BuiltinWireframe(id); !errors.Is(err, ErrNotBuiltinLayout) {
			t.Errorf("%q answered %v, want ErrNotBuiltinLayout", id, err)
		}
	}
}
