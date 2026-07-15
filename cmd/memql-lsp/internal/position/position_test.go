package position

import (
	"strings"
	"testing"
	"unicode/utf8"

	protocol "github.com/tliron/glsp/protocol_3_16"
)

// Absolute expected-value tables anchor correctness independently of the
// round-trip identity tests: a symmetric off-by-one in both FromLSP and ToLSP
// would still round-trip cleanly, so the absolute values must be pinned too.

func TestFromLSP_Absolute(t *testing.T) {
	cases := []struct {
		name             string
		content          string
		lspLine, lspChar int
		wantLine, wantCol int
	}{
		{"ascii start", "abc", 0, 0, 1, 1},
		{"ascii mid", "abc", 0, 2, 1, 3},
		{"ascii end", "abc", 0, 3, 1, 4},
		{"second line", "ab\ncd", 1, 1, 2, 2},
		{"bmp non-ascii", "café", 0, 3, 1, 4},   // é is 1 rune, 1 UTF-16 unit
		{"cjk non-ascii", "世界x", 0, 2, 1, 3},   // each CJK char is 1 rune, 1 unit
		{"astral before", "x😀y", 0, 1, 1, 2},    // char 1 == start of the emoji rune
		{"astral mid rounds down", "x😀y", 0, 2, 1, 2}, // char 2 splits the surrogate pair
		{"astral after", "x😀y", 0, 3, 1, 3},     // char 3 == 'y'
		{"astral end", "x😀y", 0, 4, 1, 4},       // char 4 == end of line
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotLine, gotCol := FromLSP(c.content, c.lspLine, c.lspChar)
			if gotLine != c.wantLine || gotCol != c.wantCol {
				t.Errorf("FromLSP(%q, %d, %d) = (%d,%d); want (%d,%d)",
					c.content, c.lspLine, c.lspChar, gotLine, gotCol, c.wantLine, c.wantCol)
			}
		})
	}
}

func TestToLSP_Absolute(t *testing.T) {
	cases := []struct {
		name               string
		content            string
		senseLine, senseCol int
		wantLine, wantChar int
	}{
		{"ascii start", "abc", 1, 1, 0, 0},
		{"ascii end", "abc", 1, 4, 0, 3},
		{"second line", "ab\ncd", 2, 2, 1, 1},
		{"bmp non-ascii", "café", 1, 4, 0, 3},
		{"astral before", "x😀y", 1, 2, 0, 1}, // col 2 (before emoji) -> UTF-16 unit 1
		{"astral after", "x😀y", 1, 3, 0, 3},  // col 3 ('y') -> UTF-16 unit 3 (emoji spans 2)
		{"astral end", "x😀y", 1, 4, 0, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotLine, gotChar := ToLSP(c.content, c.senseLine, c.senseCol)
			if gotLine != c.wantLine || gotChar != c.wantChar {
				t.Errorf("ToLSP(%q, %d, %d) = (%d,%d); want (%d,%d)",
					c.content, c.senseLine, c.senseCol, gotLine, gotChar, c.wantLine, c.wantChar)
			}
		})
	}
}

func TestByteOffsetForLSP(t *testing.T) {
	cases := []struct {
		name             string
		content          string
		line, char       int
		want             int
	}{
		{"start", "abc", 0, 0, 0},
		{"mid", "abc", 0, 2, 2},
		{"line end", "abc", 0, 3, 3},
		{"char past end clamps to line end", "abc", 0, 10, 3}, // the corruption case, fixed
		{"second line start", "ab\ncd", 1, 0, 3},
		{"second line end", "ab\ncd", 1, 2, 5},
		{"second line char past end", "ab\ncd", 1, 9, 5},
		{"line past EOF clamps to doc end", "ab\ncd", 5, 0, 5},
		{"astral before rune", "a😀b", 0, 2, 1}, // char 2 splits the pair -> rune start (byte 1)
		{"astral rune boundary", "a😀b", 0, 3, 5}, // start of 'b' (emoji is 4 bytes)
		{"astral line end", "a😀b", 0, 4, 6},
		{"empty doc", "", 0, 0, 0},
		{"empty doc line past EOF", "", 3, 3, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ByteOffsetForLSP(c.content, protocol.Position{Line: protocol.UInteger(c.line), Character: protocol.UInteger(c.char)})
			if got != c.want {
				t.Errorf("ByteOffsetForLSP(%q, {%d,%d}) = %d; want %d", c.content, c.line, c.char, got, c.want)
			}
		})
	}
}

// roundTripFixtures exercise every axis: ASCII, BMP non-ASCII, supplementary-
// plane (surrogate-pair) runes including a line that starts with one and a
// line that is only one, multi-line, trailing-newline (empty last line), and
// the empty document.
var roundTripFixtures = []string{
	"concept gadget {\n  label  string\n}\n",
	"// café 世界 mixed accents éè\nconcept école {}",
	"x😀y😀z\n😀\nplain ascii",
	"trailing newline\n",
	"",
	"single line no newline 世😀",
}

// TestRoundTrip_SenseThroughLSP walks every valid Sense position in every
// fixture line (column 1 .. runeCount+1, the end-of-line cursor) and asserts
// both identities: Sense -> LSP -> Sense returns the original Sense position,
// and the LSP boundary it produced survives LSP -> Sense -> LSP. senseCol is the
// ground truth, so this is not circular.
func TestRoundTrip_SenseThroughLSP(t *testing.T) {
	for _, content := range roundTripFixtures {
		lines := splitOnNewline(content)
		for li, line := range lines {
			senseLine := li + 1
			runeCount := utf8.RuneCountInString(line)
			for senseCol := 1; senseCol <= runeCount+1; senseCol++ {
				lspLine, lspChar := ToLSP(content, senseLine, senseCol)

				gotLine, gotCol := FromLSP(content, lspLine, lspChar)
				if gotLine != senseLine || gotCol != senseCol {
					t.Errorf("Sense->LSP->Sense drift: content=%q pos=(%d,%d) via LSP(%d,%d) -> (%d,%d)",
						content, senseLine, senseCol, lspLine, lspChar, gotLine, gotCol)
				}

				lspLine2, lspChar2 := ToLSP(content, gotLine, gotCol)
				if lspLine2 != lspLine || lspChar2 != lspChar {
					t.Errorf("LSP boundary not stable: content=%q Sense(%d,%d) -> LSP(%d,%d) but re-forward -> LSP(%d,%d)",
						content, senseLine, senseCol, lspLine, lspChar, lspLine2, lspChar2)
				}
			}
		}
	}
}

func TestMidSurrogateRoundsDownToRuneStart(t *testing.T) {
	// Character offset 2 in "x😀y" splits the emoji's surrogate pair. FromLSP
	// must round it down to the emoji's start rune (Sense column 2), matching
	// glsp's Position.IndexIn.
	line, col := FromLSP("x😀y", 0, 2)
	if line != 1 || col != 2 {
		t.Fatalf("FromLSP mid-surrogate = (%d,%d); want (1,2)", line, col)
	}
}

func TestClamping(t *testing.T) {
	cases := []struct {
		name       string
		fn         string // "from" or "to"
		content    string
		a, b       int
		wantA, wantB int
	}{
		{"from char past end", "from", "abc", 0, 100, 1, 4},
		{"from line past end", "from", "ab\ncd", 9, 0, 2, 1},
		{"from negative both", "from", "abc", -1, -5, 1, 1},
		{"from empty content", "from", "", 0, 0, 1, 1},
		{"to col past end", "to", "abc", 1, 100, 0, 3},
		{"to line past end", "to", "ab\ncd", 9, 1, 1, 0},
		{"to sense col zero", "to", "abc", 1, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotA, gotB int
			if c.fn == "from" {
				gotA, gotB = FromLSP(c.content, c.a, c.b)
			} else {
				gotA, gotB = ToLSP(c.content, c.a, c.b)
			}
			if gotA != c.wantA || gotB != c.wantB {
				t.Errorf("%s(%q, %d, %d) = (%d,%d); want (%d,%d)",
					c.fn, c.content, c.a, c.b, gotA, gotB, c.wantA, c.wantB)
			}
		})
	}
}

func TestProtocolAdapters(t *testing.T) {
	content := "x😀y\nsecond"

	// FromLSPPosition matches FromLSP on the same inputs.
	p := protocol.Position{Line: 1, Character: 3}
	gotL, gotC := FromLSPPosition(content, p)
	wantL, wantC := FromLSP(content, int(p.Line), int(p.Character))
	if gotL != wantL || gotC != wantC {
		t.Errorf("FromLSPPosition = (%d,%d); FromLSP = (%d,%d)", gotL, gotC, wantL, wantC)
	}

	// ToLSPPosition matches ToLSP on the same inputs.
	gp := ToLSPPosition(content, 1, 3)
	wl, wc := ToLSP(content, 1, 3)
	if int(gp.Line) != wl || int(gp.Character) != wc {
		t.Errorf("ToLSPPosition = (%d,%d); ToLSP = (%d,%d)", gp.Line, gp.Character, wl, wc)
	}

	// Adapter round-trip through the glsp-typed forms lands back on the origin.
	bl, bc := FromLSPPosition(content, ToLSPPosition(content, 1, 2))
	if bl != 1 || bc != 2 {
		t.Errorf("adapter round-trip = (%d,%d); want (1,2)", bl, bc)
	}
}

// splitOnNewline mirrors the package's line model for the test (Split on "\n").
func splitOnNewline(content string) []string {
	return strings.Split(content, "\n")
}
