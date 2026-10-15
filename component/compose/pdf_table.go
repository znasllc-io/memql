package compose

import (
	"math"

	"github.com/go-pdf/fpdf"
	tableast "github.com/yuin/goldmark/extension/ast"
)

func renderPDFTable(pdf *fpdf.Fpdf, table *documentTable) {
	const lineHeight, padding = 6.0, 2.0
	left, top, right, bottom := pdf.GetMargins()
	pageWidth, pageHeight := pdf.GetPageSize()
	available := pageWidth - left - right
	widths := make([]float64, len(table.alignments))
	var total float64
	for col := range widths {
		widths[col] = 16
		for row, cells := range table.rows {
			style := ""
			if row == 0 {
				style = "B"
			}
			pdf.SetFont("Helvetica", style, 11)
			widths[col] = math.Max(widths[col], math.Min(70, pdf.GetStringWidth(tr(cells[col]))+2*padding))
		}
		total += widths[col]
	}
	for col := range widths {
		widths[col] *= available / total
	}
	wrap := func(cells []string, header bool) ([][][]byte, int) {
		style := ""
		if header {
			style = "B"
		}
		pdf.SetFont("Helvetica", style, 11)
		lines := make([][][]byte, len(cells))
		count := 1
		for col, cell := range cells {
			lines[col] = pdf.SplitLines([]byte(tr(cell)), widths[col]-2*padding)
			count = max(count, len(lines[col]))
		}
		return lines, count
	}
	headerLines, headerCount := wrap(table.rows[0], true)
	headerHeight := float64(headerCount)*lineHeight + 2*padding
	// Each row is drawn in page-sized slices. Even one cell taller than a page
	// remains aligned with its siblings instead of letting MultiCell page-break
	// independently and overprint the next column.
	draw := func(lines [][][]byte, offset, count int, header bool) {
		style := ""
		if header {
			style = "B"
		}
		pdf.SetFont("Helvetica", style, 11)
		x, y := left, pdf.GetY()
		height := float64(count)*lineHeight + 2*padding
		pdf.SetDrawColor(210, 210, 210)
		for col, cell := range lines {
			pdf.Rect(x, y, widths[col], height, "D")
			align := "L"
			switch table.alignments[col] {
			case tableast.AlignRight:
				align = "R"
			case tableast.AlignCenter:
				align = "C"
			}
			for line := offset; line < min(offset+count, len(cell)); line++ {
				pdf.SetXY(x+padding, y+padding+float64(line-offset)*lineHeight)
				pdf.CellFormat(widths[col]-2*padding, lineHeight, string(cell[line]), "", 0, align, false, 0, "")
			}
			x += widths[col]
		}
		pdf.SetXY(left, y+height)
	}
	for row, cells := range table.rows {
		lines, count := wrap(cells, row == 0)
		height := float64(count)*lineHeight + 2*padding
		// Keep normal rows together; split only rows that cannot fit a page.
		capacity := pageHeight - bottom - top
		if row > 0 {
			capacity -= headerHeight
		}
		if height <= capacity && pdf.GetY()+height > pageHeight-bottom {
			pdf.AddPage()
			if row > 0 {
				draw(headerLines, 0, headerCount, true)
			}
		}
		for offset := 0; offset < count; {
			fits := int((pageHeight - bottom - pdf.GetY() - 2*padding) / lineHeight)
			if fits < 1 {
				pdf.AddPage()
				// An oversized header must not crowd every data line off the page.
				if row > 0 && headerHeight+lineHeight+2*padding <= pageHeight-bottom-top {
					draw(headerLines, 0, headerCount, true)
				}
				continue
			}
			chunk := min(fits, count-offset)
			draw(lines, offset, chunk, row == 0)
			offset += chunk
		}
	}
	pdf.SetFont("Helvetica", "", 11)
}
