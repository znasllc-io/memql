package main

import (
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql/sense"
)

func tok(typ string, sl, sc, el, ec int) sense.Token {
	return sense.Token{
		Type:  typ,
		Range: sense.Range{Start: sense.Position{Line: sl, Column: sc}, End: sense.Position{Line: el, Column: ec}},
	}
}

func TestEncodeSemanticTokens_DeltaEncoding(t *testing.T) {
	// "concept x": 'concept' at rune cols 1..8 (half-open), 'x' at 9..10.
	content := "concept x"
	data := encodeSemanticTokens(content, []sense.Token{
		tok("keyword", 1, 1, 1, 8),
		tok("identifier", 1, 9, 1, 10),
	})
	want := []protocol.UInteger{
		0, 0, 7, 0, 0, // concept: deltaLine 0, deltaChar 0, len 7, type 0 (keyword)
		0, 8, 1, 1, 0, // x: deltaLine 0, deltaChar 8, len 1, type 1 (variable)
	}
	if len(data) != len(want) {
		t.Fatalf("data len = %d; want %d (%v)", len(data), len(want), data)
	}
	for i := range want {
		if data[i] != want[i] {
			t.Errorf("data = %v; want %v", data, want)
			break
		}
	}
}

func TestEncodeSemanticTokens_MultiLineDelta(t *testing.T) {
	content := "concept a\n  field b"
	data := encodeSemanticTokens(content, []sense.Token{
		tok("keyword", 1, 1, 1, 8),   // concept
		tok("identifier", 2, 3, 2, 8), // field (line 2, cols 3..8)
	})
	// Second token starts a new line: deltaLine 1, deltaChar = absolute start (2).
	want := []protocol.UInteger{
		0, 0, 7, 0, 0,
		1, 2, 5, 1, 0,
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("data = %v; want %v", data, want)
		}
	}
}

func TestEncodeSemanticTokens_Utf16Length(t *testing.T) {
	// A token over the astral rune must report length 2 (UTF-16 code units).
	content := "a😀b"
	data := encodeSemanticTokens(content, []sense.Token{
		tok("string", 1, 2, 1, 3), // the emoji, rune cols 2..3
	})
	if len(data) != 5 {
		t.Fatalf("len=%d want 5", len(data))
	}
	// startChar: rune col 2 -> UTF-16 offset 1 ('a'); length: emoji is 2 units.
	if data[1] != 1 || data[2] != 2 {
		t.Errorf("astral token = %v; want start 1, length 2", data)
	}
}

func TestEncodeSemanticTokens_SkipsUnmappedType(t *testing.T) {
	data := encodeSemanticTokens("x", []sense.Token{tok("mystery-type", 1, 1, 1, 2)})
	if len(data) != 0 {
		t.Errorf("unmapped token type should be skipped, got %v", data)
	}
}

func TestSemanticTokensFull_Handler(t *testing.T) {
	s := newTestServerWithSense(t, sense.New(nil))
	const uri = "file:///t.memql"
	s.docs.open(uri, "concept gadget {\n  label string\n}")
	res, err := s.semanticTokensFull(nil, &protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		t.Fatalf("semanticTokensFull: %v", err)
	}
	if len(res.Data) == 0 || len(res.Data)%5 != 0 {
		t.Errorf("expected a non-empty multiple-of-5 token stream, got %d ints", len(res.Data))
	}
}

func TestSemanticTokensFull_UnknownDocument(t *testing.T) {
	s := newTestServerWithSense(t, sense.New(nil))
	res, err := s.semanticTokensFull(nil, &protocol.SemanticTokensParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///missing.memql"},
	})
	if err != nil || res == nil || len(res.Data) != 0 {
		t.Errorf("unknown document should yield empty tokens, got %+v err=%v", res, err)
	}
}

func TestSemanticTokenLegendConsistency(t *testing.T) {
	// Every legend index the map points to must be within the advertised legend.
	for typ, idx := range senseTypeToLegend {
		if int(idx) >= len(semanticTokenTypes) {
			t.Errorf("sense type %q maps to legend index %d, out of range (%d types)", typ, idx, len(semanticTokenTypes))
		}
	}
}
