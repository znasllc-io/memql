package emaildoc

import (
	"math"
	"strconv"
	"strings"
)

// Wireframe draws the structure of a layout-mode document as an SVG: grey
// outlines and bars, with no colour, no words and no pictures, so that a small
// thumbnail shows what the layout is going to be like. The drawing uses only
// the elements svg, rect, circle, line and g, and only six greys. Coordinates
// are rounded to one decimal and nothing is drawn from map order, so the same
// document always gives the same bytes.
func Wireframe(d Document) string {
	w := &wireframe{}
	y := w.blocks(d.Blocks, gutter, wfCanvas-2*gutter, 16, false)
	y += 16
	var out strings.Builder
	h := wfNum(y)
	out.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 ` + h + `" width="600" height="` + h + `">`)
	out.WriteString(`<rect x="0" y="0" width="600" height="` + h + `" fill="#ffffff"/>`)
	out.WriteString(w.b.String())
	out.WriteString(`</svg>`)
	return out.String()
}

const (
	gutter   = 24.0
	wfCanvas = 600.0

	wfPicture = "#ececec"
	wfHeading = "#d6d6d6"
	wfLine    = "#e2e2e2"
	wfOutline = "#c8c8c8"
	wfButton  = "#bdbdbd"
	wfWhite   = "#ffffff"
)

type wireframe struct{ b strings.Builder }

func wfNum(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

func (w *wireframe) rect(x, y, width, height, radius float64, fill, stroke string, dashed bool) {
	w.b.WriteString(`<rect x="` + wfNum(x) + `" y="` + wfNum(y) + `" width="` + wfNum(width) + `" height="` + wfNum(height) + `"`)
	if radius > 0 {
		w.b.WriteString(` rx="` + wfNum(radius) + `"`)
	}
	w.b.WriteString(` fill="` + fill + `"`)
	if stroke != "" {
		w.b.WriteString(` stroke="` + stroke + `" stroke-width="1" vector-effect="non-scaling-stroke"`)
		if dashed {
			w.b.WriteString(` stroke-dasharray="6 4"`)
		}
	}
	w.b.WriteString(`/>`)
}

func (w *wireframe) line(x1, y1, x2, y2 float64, stroke string) {
	w.b.WriteString(`<line x1="` + wfNum(x1) + `" y1="` + wfNum(y1) + `" x2="` + wfNum(x2) + `" y2="` + wfNum(y2) + `" stroke="` + stroke + `" stroke-width="1" vector-effect="non-scaling-stroke"/>`)
}

// place returns the left edge of an item of width iw inside [x, x+w] for the alignment.
func wfPlace(x, w, iw float64, align, def string) float64 {
	if align == "" {
		align = def
	}
	switch align {
	case "left":
		return x
	case "right":
		return x + w - iw
	}
	return x + (w-iw)/2
}

// bar draws one rounded bar of the given width, aligned inside [x, x+w].
func (w *wireframe) bar(x, y, width, bw, height float64, fill, align string) {
	w.rect(wfPlace(x, width, bw, align, "left"), y, bw, height, 2, fill, "", false)
}

// blocks draws a column of blocks starting at y and returns the y below them.
// top is the top-level call, where a section spans the whole canvas.
func (w *wireframe) blocks(blocks []Block, x, width, y float64, nested bool) float64 {
	for _, b := range blocks {
		y = w.block(b, x, width, y, nested)
	}
	return y
}

func (w *wireframe) block(b Block, x, width, y float64, nested bool) float64 {
	switch b.Type {
	case "image":
		iw := float64(b.Width)
		if b.Bleed {
			// A bleed picture spans the canvas, whatever column it is in at the top.
			iw = wfCanvas
			x, width = 0, wfCanvas
		}
		if iw <= 0 || iw > width {
			iw = width
		}
		ih := b.Height
		if ih <= 0 {
			ih = iw * 0.5
		}
		if b.Width > 0 && float64(b.Width) > width {
			ih = ih * iw / float64(b.Width)
		}
		ix := wfPlace(x, width, iw, b.Align, "center")
		w.rect(ix, y, iw, ih, 0, wfPicture, wfOutline, false)
		w.line(ix, y, ix+iw, y+ih, wfOutline)
		w.line(ix+iw, y, ix, y+ih, wfOutline)
		return y + ih + 8
	case "heading":
		size := b.Size
		if size <= 0 {
			size = 24
		}
		lines := int(math.Ceil(float64(len([]rune(b.Text))) * size * 0.5 / width))
		lines = min(max(lines, 1), 3)
		step := b.LineHeight
		if step < size {
			step = size * 1.15
		}
		bh := 0.6 * size
		y += 4
		for i := 0; i < lines; i++ {
			bw := width
			if lines == 1 {
				bw = width * 0.7
			} else if i == lines-1 {
				bw = width * 0.55
			}
			w.bar(x, y+(step-bh)/2, width, bw, bh, wfHeading, b.Align)
			y += step
		}
		return y + 4
	case "text":
		y += 4
		for i, f := range [3]float64{1, 1, 0.65} {
			w.bar(x, y, width, width*f, 6, wfLine, b.Align)
			if i < 2 {
				y += 10
			}
		}
		return y + 6 + 8
	case "highlight":
		w.rect(x, y, width, 52, 0, wfPicture, wfOutline, false)
		w.bar(x+16, y+14, width-32, width-32, 6, wfLine, "left")
		w.bar(x+16, y+30, width-32, (width-32)*0.6, 6, wfLine, "left")
		return y + 52 + 8
	case "button":
		bw := float64(b.Width)
		if bw <= 0 || bw > width {
			bw = math.Min(300, width)
		}
		w.rect(wfPlace(x, width, bw, b.Align, "center"), y+4, bw, 40, 20, wfButton, "", false)
		return y + 48 + 4
	case "divider":
		w.line(x, y+8, x+width, y+8, wfOutline)
		return y + 16
	case "spacer":
		h := b.Height
		if h <= 0 {
			h = 16
		}
		return y + h
	case "social":
		n := len(b.Links)
		if n == 0 {
			n = 3
		}
		span := float64(n-1)*32 + 20
		left := wfPlace(x, width, span, b.Align, "center")
		for i := 0; i < n; i++ {
			cx := left + 10 + float64(i)*32
			w.b.WriteString(`<circle cx="` + wfNum(cx) + `" cy="` + wfNum(y+16) + `" r="10" fill="` + wfLine + `" stroke="` + wfOutline + `" stroke-width="1" vector-effect="non-scaling-stroke"/>`)
		}
		return y + 32 + 8
	case "footer":
		y += 8
		for _, f := range [5]float64{0.5, 0.35, 0.8, 0.6, 0.4} {
			w.bar(x, y, width, width*f, 6, wfLine, "center")
			y += 10
		}
		return y + 8
	case "html":
		w.rect(x, y, width, 56, 0, wfWhite, wfOutline, true)
		return y + 56 + 8
	case "section":
		top := y
		inX, inW := x, width
		if !nested {
			x, width = 0, wfCanvas
			inX, inW = gutter, wfCanvas-2*gutter
		} else {
			inX, inW = x+8, width-16
		}
		// Draw children into a scratch buffer so the outline sits behind them.
		child := &wireframe{}
		end := child.blocks(b.Blocks, inX, inW, top+12, true)
		end += 12
		w.rect(x+0.5, top+0.5, width-1, end-top-1, 0, wfWhite, wfOutline, false)
		w.b.WriteString(child.b.String())
		return end + 8
	case "columns":
		total := 0.0
		for _, c := range b.Columns {
			total += c.Width
		}
		if total <= 0 {
			return y
		}
		maxEnd := y
		cx := x
		for _, c := range b.Columns {
			cw := width * c.Width / total
			end := w.blocks(c.Blocks, cx+6, cw-12, y, true)
			maxEnd = math.Max(maxEnd, end)
			cx += cw
		}
		return maxEnd
	}
	return y
}
