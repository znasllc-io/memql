package compose

import (
	"fmt"
	"strings"
	"unicode/utf8"

	tableast "github.com/yuin/goldmark/extension/ast"
)

func writeDOCXTable(b *strings.Builder, table *documentTable) {
	const pageWidth = 9026 // A4 minus the document's two 1440-twip margins.
	widths := make([]int, len(table.alignments))
	total := 0
	for col := range widths {
		widths[col] = 8
		for _, row := range table.rows {
			widths[col] = max(widths[col], min(40, utf8.RuneCountInString(row[col])+2))
		}
		total += widths[col]
	}
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="9026" w:type="dxa"/><w:tblLayout w:type="fixed"/><w:tblBorders>`)
	for _, edge := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(b, `<w:%s w:val="single" w:sz="4" w:color="D2D2D2"/>`, edge)
	}
	b.WriteString(`</w:tblBorders><w:tblCellMar><w:top w:w="100" w:type="dxa"/><w:left w:w="100" w:type="dxa"/><w:bottom w:w="100" w:type="dxa"/><w:right w:w="100" w:type="dxa"/></w:tblCellMar></w:tblPr><w:tblGrid>`)
	used := 0
	for col := range widths {
		widths[col] = widths[col] * pageWidth / total
		if col == len(widths)-1 {
			widths[col] = pageWidth - used
		}
		used += widths[col]
		fmt.Fprintf(b, `<w:gridCol w:w="%d"/>`, widths[col])
	}
	b.WriteString(`</w:tblGrid>`)
	for row, cells := range table.rows {
		b.WriteString(`<w:tr>`)
		if row == 0 {
			b.WriteString(`<w:trPr><w:tblHeader/></w:trPr>`)
		}
		for col, cell := range cells {
			fmt.Fprintf(b, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/></w:tcPr><w:p><w:pPr><w:spacing w:before="0" w:after="0"/>`, widths[col])
			align := "left"
			switch table.alignments[col] {
			case tableast.AlignRight:
				align = "right"
			case tableast.AlignCenter:
				align = "center"
			}
			fmt.Fprintf(b, `<w:jc w:val="%s"/></w:pPr><w:r>`, align)
			if row == 0 {
				b.WriteString(`<w:rPr><w:b/></w:rPr>`)
			}
			for i, line := range strings.Split(cell, "\n") {
				if i > 0 {
					b.WriteString(`<w:br/>`)
				}
				b.WriteString(`<w:t xml:space="preserve">` + escapeXML(line) + `</w:t>`)
			}
			b.WriteString(`</w:r></w:p></w:tc>`)
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl><w:p/>`)
}
