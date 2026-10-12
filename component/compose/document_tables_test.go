package compose

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
	"testing"
)

const orchardTable = "Before the table.\n\n| Fruit | Count |\n| --- | ---: |\n| Apples | 7 |\n| Pears | 5 |\n\nTotal: 12"

func TestPDFRendersMarkdownTableAsAlignedCells(t *testing.T) {
	result, err := Render(FormatPDF, Draft{Title: "Orchard QA", Body: orchardTable}, fixtureProvenance())
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Join(pdfPageStreams(t, result.Bytes), "\n")
	for _, want := range []string{"Orchard QA", "Before the table.", "Fruit", "Count", "Apples", "7", "Pears", "5", "Total: 12"} {
		if !strings.Contains(content, "("+want+")") {
			t.Errorf("PDF lost %q", want)
		}
	}
	if strings.Contains(content, "| ---") || strings.Contains(content, "| Fruit") {
		t.Fatal("PDF printed Markdown table source instead of rendering cells")
	}
	// Inspect actual PDF drawing/text operators, not the intermediate table.
	if got := strings.Count(content, " re S"); got != 6 {
		t.Fatalf("PDF drew %d cells, want 3 rows of 2", got)
	}
	position := regexp.MustCompile(`BT ([\d.]+) ([\d.]+) Td \((Fruit|Count|Apples|7|Pears|5)\)\s*Tj ET`)
	positions := map[string][]string{}
	for _, match := range position.FindAllStringSubmatch(content, -1) {
		positions[match[3]] = match[1:3]
	}
	for _, row := range [][2]string{{"Fruit", "Count"}, {"Apples", "7"}, {"Pears", "5"}} {
		a, b := positions[row[0]], positions[row[1]]
		if len(a) != 2 || len(b) != 2 || a[0] == b[0] || a[1] != b[1] {
			t.Errorf("table row %v is not in distinct aligned cells: %v, %v", row, a, b)
		}
	}
}

func TestPDFTableWrapsAndPaginatesWithoutLosingCellText(t *testing.T) {
	// One row exceeds a page; ordinary rows after it also force page breaks.
	body := "| Description | Count |\n| --- | ---: |\n| " + strings.Repeat("wrapped words ", 1500) + "last-word | 17 |\n"
	for range 50 {
		body += "| Ordinary row | 3 |\n"
	}
	body += "\nAfter the table."
	result, err := Render(FormatPDF, Draft{Body: body}, fixtureProvenance())
	if err != nil {
		t.Fatal(err)
	}
	pages := pdfPageStreams(t, result.Bytes)
	if len(pages) < 3 {
		t.Fatalf("long table did not paginate: %d pages", len(pages))
	}
	content := strings.Join(pages, "\n")
	for want, count := range map[string]int{"wrapped": 1500, "last-word": 1, "(17)": 1, "Ordinary row": 50, "After the table.": 1} {
		if got := strings.Count(content, want); got != count {
			t.Errorf("PDF contains %d occurrences of %q, want %d", got, want, count)
		}
	}
	for i, page := range pages {
		if !strings.Contains(page, "(Description)") || !strings.Contains(page, "(Count)") {
			t.Errorf("page %d lost the repeated table header", i+1)
		}
	}
}

func TestDOCXRendersMarkdownTableAsWordCells(t *testing.T) {
	result, err := Render(FormatDOCX, Draft{Title: "Orchard QA", Body: orchardTable}, fixtureProvenance())
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(result.Bytes), int64(len(result.Bytes)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := z.Open("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := xml.NewDecoder(f)
	var cells []string
	var cell strings.Builder
	inCell, rows, tables := false, 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch v := token.(type) {
		case xml.StartElement:
			switch v.Name.Local {
			case "tbl":
				tables++
			case "tr":
				rows++
			case "tc":
				inCell = true
				cell.Reset()
			}
		case xml.EndElement:
			if v.Name.Local == "tc" {
				cells = append(cells, cell.String())
				inCell = false
			}
		case xml.CharData:
			if inCell {
				cell.Write(v)
			}
		}
	}
	if tables != 1 || rows != 3 || strings.Join(cells, ",") != "Fruit,Count,Apples,7,Pears,5" {
		t.Fatalf("Word table = %d tables, %d rows, cells %q", tables, rows, cells)
	}
}

func TestDocumentTablesUseGFMParsingAndPreserveOtherSource(t *testing.T) {
	body := "Intro\n| Item | Detail |\n| --- | --- |\n| **A** \\| B | [link](https://example.com) &amp; `code` |\n\n" +
		"```\n| Example | Value |\n| --- | --- |\n```\n\n| Empty | Header |\n| --- | --- |"
	blocks := documentBlocks(body)
	if len(blocks) != 4 || blocks[0].paragraph != "Intro" || blocks[1].table == nil || blocks[2].paragraph != "```\n| Example | Value |\n| --- | --- |\n```" || blocks[3].table == nil {
		t.Fatalf("wrong block boundaries: %+v", blocks)
	}
	if got := strings.Join(blocks[1].table.rows[1], ";"); got != "A | B;link & code" {
		t.Fatalf("inline table text = %q", got)
	}
	if len(blocks[3].table.rows) != 1 {
		t.Fatal("header-only table lost its header")
	}
}

func pdfPageStreams(t *testing.T, pdf []byte) []string {
	t.Helper()
	var streams []string
	for _, match := range regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`).FindAllSubmatch(pdf, -1) {
		reader, err := zlib.NewReader(bytes.NewReader(match[1]))
		if err != nil {
			continue // Uncompressed XMP metadata is not a page content stream.
		}
		content, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, string(content))
	}
	if len(streams) == 0 {
		t.Fatal("PDF has no readable page content streams")
	}
	return streams
}
