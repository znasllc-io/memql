package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Drift guard over the VS Code activity-bar icon (znasllc-io/memql#3342).
//
// THE DEFECT. The activity-bar glyph was not the memQL mark. It was a
// hand-drawn 4-node diamond, while the brand logo shipped alongside it
// (icons/memql.png, used for the .memql file icon in the Explorer) is a 9-node
// polyhedral graph with a pendant node. Side by side in the same window the
// glyph read as a fragment of the logo -- "it's all cut off, it looks like part
// of it".
//
// WHY THE OBVIOUS GUARD WOULD NOT HAVE CAUGHT IT, which is the part worth
// writing down. The natural reading of "cut off" is clipping, so the natural
// test is "does the geometry fit inside the viewBox". The old icon FIT ITS
// VIEWBOX PERFECTLY (x 1.6..22.4, y 3.6..22.4 of 0..24). Nothing was clipped.
// A fit check would have passed on the broken asset and taught us the icon was
// fine. The thing that was actually wrong is that the glyph had drifted from
// the logo, so that is what this asserts: the activity icon must be the SAME
// GRAPH as icons/memql.png.
//
// It does that by tracing the PNG -- eroding the ink to isolate the node discs,
// which are wider than the strokes -- and matching those node centres against
// the SVG's circles under a normalizing fit. Both assets can be re-rendered,
// rescaled or recoloured freely; only a change to the GRAPH ITSELF fails, and
// then it fails for both a new logo and a re-drawn glyph, which is exactly when
// a human should be looking.
//
// The fit check is kept as well, because it is cheap and it is the property the
// bug report named even though it was not the cause.
const (
	extensionDir     = "../../editors/vscode"
	brandLogoPNG     = "../../editors/vscode/icons/memql.png"
	svgViewBoxTarget = 24.0
)

type svgDoc struct {
	ViewBox string `xml:"viewBox,attr"`
	Groups  []struct {
		StrokeWidth string `xml:"stroke-width,attr"`
		Lines       []struct {
			X1 float64 `xml:"x1,attr"`
			Y1 float64 `xml:"y1,attr"`
			X2 float64 `xml:"x2,attr"`
			Y2 float64 `xml:"y2,attr"`
		} `xml:"line"`
		Circles []struct {
			CX float64 `xml:"cx,attr"`
			CY float64 `xml:"cy,attr"`
			R  float64 `xml:"r,attr"`
		} `xml:"circle"`
	} `xml:"g"`
}

// activityIconPath reads the icon the manifest actually contributes, rather
// than hardcoding a filename: renaming the asset must not silently retarget
// this guard at a file nobody ships.
func activityIconPath(t *testing.T) string {
	t.Helper()
	var manifest struct {
		Contributes struct {
			ViewsContainers struct {
				ActivityBar []struct {
					ID   string `json:"id"`
					Icon string `json:"icon"`
				} `json:"activitybar"`
			} `json:"viewsContainers"`
		} `json:"contributes"`
	}
	raw, err := os.ReadFile(filepath.Join(extensionDir, "package.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("unmarshal extension manifest: %v", err)
	}
	bars := manifest.Contributes.ViewsContainers.ActivityBar
	if len(bars) == 0 {
		t.Fatal("the manifest contributes no activity-bar container; this guard " +
			"cannot pass vacuously (#3342)")
	}
	if bars[0].Icon == "" {
		t.Fatalf("activity-bar container %q declares no icon (#3342)", bars[0].ID)
	}
	return filepath.Join(extensionDir, bars[0].Icon)
}

func loadActivityIcon(t *testing.T) svgDoc {
	t.Helper()
	raw, err := os.ReadFile(activityIconPath(t))
	if err != nil {
		t.Fatalf("read activity icon: %v", err)
	}
	var doc svgDoc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse activity icon as XML: %v", err)
	}
	return doc
}

// iconNodes returns the SVG's node discs.
func iconNodes(t *testing.T, doc svgDoc) [][2]float64 {
	t.Helper()
	var out [][2]float64
	for _, g := range doc.Groups {
		for _, c := range g.Circles {
			out = append(out, [2]float64{c.CX, c.CY})
		}
	}
	if len(out) == 0 {
		t.Fatal("the activity icon declares no <circle> nodes; this guard cannot " +
			"pass vacuously (#3342)")
	}
	return out
}

// The glyph must be the same graph as the brand logo. This is the assertion
// that fails against the 4-node diamond that shipped.
func TestActivityIconIsTheBrandMark(t *testing.T) {
	logo := traceLogoNodes(t)
	icon := iconNodes(t, loadActivityIcon(t))

	if len(icon) != len(logo) {
		t.Fatalf("the activity-bar icon has %d nodes but the brand logo "+
			"(icons/memql.png) has %d.\n"+
			"The activity glyph must BE the memQL mark, not an approximation of "+
			"it: the two render side by side -- the glyph in the activity bar, the "+
			"logo on every .memql file in the Explorer -- and a simplified glyph "+
			"reads as a broken or cropped logo (#3342).", len(icon), len(logo))
	}

	// Compare under a normalizing fit so scale and translation are free: the
	// glyph is drawn on a 24x24 canvas and the logo on 256x256, and refitting
	// either is a legitimate change. Only the SHAPE of the graph is asserted.
	ni := normalize(icon)
	nl := normalize(logo)
	sort.Slice(ni, func(a, b int) bool { return lexLess(ni[a], ni[b]) })
	sort.Slice(nl, func(a, b int) bool { return lexLess(nl[a], nl[b]) })

	// Tolerance in normalized units (the mark's own bounding box is 1.0 wide).
	// 0.04 is ~1px on the 24px canvas -- tight enough that a different graph
	// cannot pass, loose enough to absorb re-tracing and rounding.
	const tol = 0.04
	for i := range ni {
		dx := ni[i][0] - nl[i][0]
		dy := ni[i][1] - nl[i][1]
		if d := math.Hypot(dx, dy); d > tol {
			t.Errorf("node %d of the activity icon sits at normalized (%.3f, %.3f) "+
				"but the corresponding logo node is at (%.3f, %.3f) -- offset %.3f, "+
				"tolerance %.3f.\nThe glyph has drifted from the brand mark (#3342).",
				i, ni[i][0], ni[i][1], nl[i][0], nl[i][1], d, tol)
		}
	}
}

// normalize maps a point set onto its own bounding box, so two drawings of the
// same graph at different scales and offsets compare equal.
func normalize(pts [][2]float64) [][2]float64 {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
		minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
	}
	// One scale for both axes, so the aspect ratio is part of what is compared;
	// squashing the mark is a real change, not a normalization artefact.
	s := math.Max(maxX-minX, maxY-minY)
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[i] = [2]float64{(p[0] - minX) / s, (p[1] - minY) / s}
	}
	return out
}

func lexLess(a, b [2]float64) bool {
	if math.Abs(a[1]-b[1]) > 0.02 {
		return a[1] < b[1]
	}
	return a[0] < b[0]
}

// ...and it must still fit the canvas. Cheap, and it is the property the bug
// report named: the whole mark inside the viewBox, touching no edge.
func TestActivityIconFitsItsViewBox(t *testing.T) {
	doc := loadActivityIcon(t)
	if doc.ViewBox != "0 0 24 24" {
		t.Errorf("activity icon viewBox = %q, want \"0 0 24 24\" (the canvas VS "+
			"Code lays activity-bar icons out on)", doc.ViewBox)
	}

	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	grow := func(x, y, pad float64) {
		minX, maxX = math.Min(minX, x-pad), math.Max(maxX, x+pad)
		minY, maxY = math.Min(minY, y-pad), math.Max(maxY, y+pad)
	}

	var prims int
	for _, g := range doc.Groups {
		// Round caps extend a line by half its stroke width past the endpoint,
		// so the drawn extent is wider than the coordinates alone imply --
		// exactly the margin a "the numbers look fine" reading would miss.
		var half float64
		if g.StrokeWidth != "" {
			var w float64
			if _, err := fmt.Sscanf(g.StrokeWidth, "%f", &w); err == nil {
				half = w / 2
			}
		}
		for _, l := range g.Lines {
			grow(l.X1, l.Y1, half)
			grow(l.X2, l.Y2, half)
			prims++
		}
		for _, c := range g.Circles {
			grow(c.CX, c.CY, c.R)
			prims++
		}
	}
	if prims == 0 {
		t.Fatal("the activity icon declares no drawable primitives; this guard " +
			"cannot pass vacuously (#3342)")
	}

	if minX < 0 || minY < 0 || maxX > svgViewBoxTarget || maxY > svgViewBoxTarget {
		t.Errorf("the activity icon's drawn extent is x %.2f..%.2f y %.2f..%.2f, "+
			"which escapes the 0..24 viewBox -- it would render clipped (#3342)",
			minX, maxX, minY, maxY)
	}

	// A mark that fits but hides in a corner is the same complaint wearing a
	// different shape, so require it to actually occupy the canvas and sit
	// roughly centred.
	if w, h := maxX-minX, maxY-minY; w < 16 || h < 16 {
		t.Errorf("the activity icon occupies only %.1fx%.1f of its 24x24 canvas; "+
			"it would render small and lost next to VS Code's own icons (#3342)", w, h)
	}
	cx, cy := (minX+maxX)/2, (minY+maxY)/2
	if math.Abs(cx-12) > 1.0 || math.Abs(cy-12) > 1.0 {
		t.Errorf("the activity icon's centre is (%.2f, %.2f), more than 1 unit off "+
			"the canvas centre (12, 12); it would render visibly off-centre (#3342)",
			cx, cy)
	}
}

// traceLogoNodes recovers the node centres of the brand logo from the shipped
// PNG.
//
// Node discs are substantially wider than the connecting strokes, so a distance
// transform followed by a threshold between the two widths isolates the discs
// and nothing else. Deriving the reference from the artwork -- rather than
// pasting coordinates into this file -- is what makes the guard bidirectional:
// re-draw the logo and the glyph is required to follow.
func traceLogoNodes(t *testing.T) [][2]float64 {
	t.Helper()
	f, err := os.Open(brandLogoPNG)
	if err != nil {
		t.Fatalf("open brand logo: %v", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode brand logo: %v", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	ink := make([][]bool, h)
	for y := 0; y < h; y++ {
		ink[y] = make([]bool, w)
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			if a < 0x8000 {
				continue
			}
			lum := (299*int(r>>8) + 587*int(g>>8) + 114*int(bl>>8)) / 1000
			ink[y][x] = lum < 200
		}
	}

	// Chamfer distance transform (5/7 weights, i.e. distance x5).
	const inf = 1 << 20
	dist := make([][]int, h)
	for y := range dist {
		dist[y] = make([]int, w)
		for x := range dist[y] {
			if ink[y][x] {
				dist[y][x] = inf
			}
		}
	}
	relax := func(x, y int, deltas [][3]int) {
		if dist[y][x] == 0 {
			return
		}
		best := dist[y][x]
		for _, d := range deltas {
			nx, ny := x+d[0], y+d[1]
			if nx < 0 || ny < 0 || nx >= w || ny >= h {
				if d[2] < best {
					best = d[2]
				}
				continue
			}
			if v := dist[ny][nx] + d[2]; v < best {
				best = v
			}
		}
		dist[y][x] = best
	}
	fwd := [][3]int{{-1, 0, 5}, {0, -1, 5}, {-1, -1, 7}, {1, -1, 7}}
	bwd := [][3]int{{1, 0, 5}, {0, 1, 5}, {1, 1, 7}, {-1, 1, 7}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			relax(x, y, fwd)
		}
	}
	for y := h - 1; y >= 0; y-- {
		for x := w - 1; x >= 0; x-- {
			relax(x, y, bwd)
		}
	}

	var maxd int
	for y := range dist {
		for x := range dist[y] {
			if dist[y][x] > maxd && dist[y][x] < inf {
				maxd = dist[y][x]
			}
		}
	}
	if maxd == 0 {
		t.Fatal("found no ink in the brand logo; this guard cannot pass vacuously (#3342)")
	}
	thr := maxd * 55 / 100

	seen := make([][]bool, h)
	for y := range seen {
		seen[y] = make([]bool, w)
	}
	var centres [][2]float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if dist[y][x] < thr || seen[y][x] {
				continue
			}
			var sx, sy float64
			var n int
			stack := [][2]int{{x, y}}
			seen[y][x] = true
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				sx += float64(p[0])
				sy += float64(p[1])
				n++
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						nx, ny := p[0]+dx, p[1]+dy
						if nx < 0 || ny < 0 || nx >= w || ny >= h || seen[ny][nx] || dist[ny][nx] < thr {
							continue
						}
						seen[ny][nx] = true
						stack = append(stack, [2]int{nx, ny})
					}
				}
			}
			if n >= 8 {
				centres = append(centres, [2]float64{sx / float64(n), sy / float64(n)})
			}
		}
	}
	if len(centres) == 0 {
		t.Fatal("traced no nodes from the brand logo; this guard cannot pass vacuously (#3342)")
	}
	return centres
}
