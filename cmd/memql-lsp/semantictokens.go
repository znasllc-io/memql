package main

import (
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// semanticTokenTypes is the legend advertised at initialize: standard LSP
// semantic-token types so a client's default theme colors them without extra
// mapping. senseTypeToLegend projects each Sense token-type string onto an index
// into this slice.
var semanticTokenTypes = []string{
	"keyword",   // 0
	"variable",  // 1
	"string",    // 2
	"number",    // 3
	"operator",  // 4
	"decorator", // 5
	"comment",   // 6
	"type",      // 7
}

var senseTypeToLegend = map[string]uint32{
	"keyword":     0,
	"identifier":  1,
	"string":      2,
	"number":      3,
	"operator":    4,
	"annotation":  5,
	"comment":     6,
	"concept":     7,
	"punctuation": 4, // no standard "punctuation" type -- color as operator
}

func (s *server) semanticTokensFull(_ *glsp.Context, params *protocol.SemanticTokensParams) (*protocol.SemanticTokens, error) {
	empty := &protocol.SemanticTokens{Data: []protocol.UInteger{}}
	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return empty, nil
	}
	svc := s.getSense()
	if svc == nil {
		return empty, nil
	}
	return &protocol.SemanticTokens{Data: encodeSemanticTokens(text, svc.Tokenize(text))}, nil
}

// encodeSemanticTokens delta-packs Sense tokens into the LSP semantic-tokens
// wire format: five uint32 per token -- deltaLine, deltaStartChar, length,
// tokenType, tokenModifiers -- each position relative to the previous token, in
// LSP 0-based UTF-16 coordinates. Tokens whose type is unmapped, or which would
// encode a non-positive length or a backwards delta, are skipped.
func encodeSemanticTokens(content string, toks []sense.Token) []protocol.UInteger {
	data := make([]protocol.UInteger, 0, len(toks)*5)
	prevLine, prevChar := 0, 0
	for _, tk := range toks {
		idx, ok := senseTypeToLegend[tk.Type]
		if !ok {
			continue
		}
		startLine, startChar := position.ToLSP(content, tk.Range.Start.Line, tk.Range.Start.Column)

		var length int
		if tk.Range.End.Line == tk.Range.Start.Line {
			_, endChar := position.ToLSP(content, tk.Range.End.Line, tk.Range.End.Column)
			length = endChar - startChar
		} else {
			// Multi-line token: clamp to the end of its start line (a huge
			// column clamps to line end via the position layer).
			_, eol := position.ToLSP(content, tk.Range.Start.Line, 1<<30)
			length = eol - startChar
		}
		if length <= 0 {
			continue
		}

		deltaLine := startLine - prevLine
		deltaChar := startChar
		if deltaLine == 0 {
			deltaChar = startChar - prevChar
		}
		if deltaLine < 0 || deltaChar < 0 {
			continue // tokens out of order: cannot delta-encode
		}

		data = append(data,
			protocol.UInteger(deltaLine),
			protocol.UInteger(deltaChar),
			protocol.UInteger(length),
			protocol.UInteger(idx),
			0, // no modifiers
		)
		prevLine, prevChar = startLine, startChar
	}
	return data
}
