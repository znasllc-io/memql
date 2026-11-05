package emaildoc

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestContrastRatio(t *testing.T) {
	for _, c := range []struct {
		fg, bg string
		want   float64
	}{
		{"#000000", "#ffffff", 21},
		{"#ffffff", "#000000", 21},
		{"#ffffff", "#ffffff", 1},
		{"#767676", "#ffffff", 4.54}, // the lightest grey that passes AA on white
		{"#777777", "#ffffff", 4.48}, // and the next one, which does not
		{"#0000ff", "#ffffff", 8.59},
	} {
		if got := ContrastRatio(c.fg, c.bg); math.Abs(got-c.want) > 0.005 {
			t.Errorf("%s on %s: want %.2f, got %.4f", c.fg, c.bg, c.want, got)
		}
	}
}

func TestTextRunsReadWhatAReaderSees(t *testing.T) {
	runs, err := TextRuns(`<!DOCTYPE html><html><head><title>t</title><style>td{color:#ff0000}</style></head>` +
		`<body style="background-color:#eef0f3"><div style="display:none;color:#eef0f3">hidden</div>` +
		`<table style="background-color:#223344"><tbody><tr>` +
		`<td data-block="big" style="color:#111;font-size:30px;font-weight:700">Big <span style="font-weight:normal">plain</span></td>` +
		`<td data-block="small" bgcolor="#000000"><span style="color:#FFF;font-size:12.5px">Small</span></td>` +
		`<td style="font-size:0">&nbsp;</td><td style="font-size:0">gone</td>` +
		`</tr></tbody></table><!-- <p style="color:#ff0000">in a comment</p> --></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%g|%v", r.Block, r.Words, r.Foreground, r.Background, r.Size, r.Bold))
	}
	want := []string{
		"big|Big|#111111|#223344|30|true",
		"big|plain|#111111|#223344|30|false",
		"small|Small|#ffffff|#000000|12.5|false",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("want\n%s\ngot\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
	if !runs[0].Large() || !runs[1].Large() || runs[2].Large() {
		t.Errorf("30px is large text, bold or not, and 12.5px is not: %+v", runs)
	}
}

// TestLargeTextNeedsLessContrast pins WCAG's large text: 24 pixels at any
// weight, or 18.66 pixels (14 points) and bold, which need 3 to 1; anything
// smaller needs 4.5 to 1.
func TestLargeTextNeedsLessContrast(t *testing.T) {
	for _, c := range []struct {
		size  float64
		bold  bool
		large bool
	}{
		{23.9, false, false}, {24, false, true}, {40, false, true},
		{18.65, true, false}, {18.66, true, true}, {18.66, false, false}, {18.7, false, false},
		{17.3, true, false}, {14.7, true, false}, {14.7, false, false},
	} {
		run := TextRun{Size: c.size, Bold: c.bold}
		want := 4.5
		if c.large {
			want = 3
		}
		if run.Large() != c.large || run.Required() != want {
			t.Errorf("%gpx bold=%v: want large=%v needing %g to 1, got large=%v needing %g", c.size, c.bold, c.large, want, run.Large(), run.Required())
		}
	}
}

// roleNames says which colours of the theme a colour is, for a message a
// person can act on: "secondary" and not "#5b8fb9".
func roleNames(theme Theme, hex string) string {
	var names []string
	for _, role := range []struct{ name, value string }{
		{"page", theme.Page}, {"canvas", theme.Canvas}, {"primary", theme.Primary}, {"secondary", theme.Secondary},
		{"accent", theme.Accent}, {"tint", theme.Tint}, {"text", theme.Text}, {"muted", theme.Muted}, {"white", "#ffffff"},
	} {
		if strings.EqualFold(role.value, hex) {
			names = append(names, role.name)
		}
	}
	if len(names) == 0 {
		return hex
	}
	return strings.Join(names, "/") + " " + hex
}

// unreadBlocks lists, in document order, the blocks that have words (a
// heading, a text, a highlight, a button, a social block or a footer) of which
// no words are among the runs.
func unreadBlocks(blocks []Block, runs []TextRun) []string {
	read := map[string]bool{}
	for _, run := range runs {
		read[run.Block] = true
	}
	var silent []string
	walkBlocks(blocks, func(b Block) bool {
		switch b.Type {
		case typeHeading, typeText, typeHighlight, typeButton, typeSocial, typeFooter:
			if !read[b.ID] {
				silent = append(silent, b.ID)
			}
		}
		return true
	})
	return silent
}

func TestUnreadBlocksAreNoticed(t *testing.T) {
	blocks := []Block{
		{ID: "h", Type: typeHeading}, {ID: "t", Type: typeText}, {ID: "l", Type: typeHighlight},
		{ID: "b", Type: typeButton}, {ID: "s", Type: typeSocial}, {ID: "f", Type: typeFooter},
		{ID: "i", Type: typeImage}, {ID: "d", Type: typeDivider}, {ID: "p", Type: typeSpacer},
		{ID: "sec", Type: typeSection, Blocks: []Block{{ID: "inner", Type: typeText}}},
	}
	if got := strings.Join(unreadBlocks(blocks, []TextRun{{Block: "h"}}), " "); got != "t l b s f inner" {
		t.Errorf("want every block with words but the heading, got %q", got)
	}
	runs := []TextRun{{Block: "h"}, {Block: "t"}, {Block: "l"}, {Block: "b"}, {Block: "s"}, {Block: "f"}, {Block: "inner"}}
	if got := unreadBlocks(blocks, runs); len(got) != 0 {
		t.Errorf("every block with words was read, got %v", got)
	}
}

// TestBuiltinLayoutsMeetTextContrast checks every pair of words and the colour
// behind them that each built-in layout draws, as the renderer writes them
// (so the theme's roles as the blocks use them, the defaults of each block
// type included): 4.5 to 1 for text, 3 to 1 for large text (24 pixels, or
// 18.66 pixels and bold), the WCAG 2 level AA. These are the layouts people
// start from, and what they leave alone is what gets sent.
func TestBuiltinLayoutsMeetTextContrast(t *testing.T) {
	for _, info := range Layouts() {
		t.Run(info.ID, func(t *testing.T) {
			r, err := Render(context.Background(), info.Document, nil, RenderOptions{Preview: true})
			if err != nil {
				t.Fatal(err)
			}
			runs, err := TextRuns(r.HTML)
			if err != nil {
				t.Fatal(err)
			}
			reported := map[string]bool{}
			for _, run := range runs {
				got := ContrastRatio(run.Foreground, run.Background)
				if got >= run.Required() {
					continue
				}
				// One report for each block and colours, however many words.
				key := fmt.Sprintf("%s|%s|%s|%g|%v", run.Block, run.Foreground, run.Background, run.Size, run.Bold)
				if reported[key] {
					continue
				}
				reported[key] = true
				kind := "text"
				if run.Large() {
					kind = "large text"
				}
				weight := ""
				if run.Bold {
					weight = ", bold"
				}
				t.Errorf("block %q: %s (%gpx%s) is %s on %s: %.2f to 1, %s needs %.1f to 1",
					run.Block, strconv.Quote(clip(run.Words, 30)), run.Size, weight,
					roleNames(info.Document.Theme, run.Foreground), roleNames(info.Document.Theme, run.Background), got, kind, run.Required())
			}
			// A check that read no words would pass for nothing: every block
			// that has words must have been read.
			if silent := unreadBlocks(info.Document.Blocks, runs); len(silent) > 0 {
				t.Errorf("no words of these blocks were read, so none were checked: %v", silent)
			}
		})
	}
}

func TestContrastRatioKnownValues(t *testing.T) {
	if got := ContrastRatio("#ffffff", "#000000"); math.Abs(got-21) > 0.01 {
		t.Fatalf("white/black %v", got)
	}
	if got := ContrastRatio("#ffffff", "#55b9bd"); math.Abs(got-2.32) > 0.02 {
		t.Fatalf("white/teal %v", got)
	}
}
