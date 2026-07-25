package main

import (
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
)

func (s *server) hover(_ *glsp.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	svc := s.getSense()
	if svc == nil {
		return nil, nil
	}
	line, col := position.FromLSPPosition(text, params.Position)
	res := svc.Hover(text, line, col, uriToPath(params.TextDocument.URI))
	if res == nil {
		return nil, nil
	}
	rng := toLSPRange(text, res.Range)
	return &protocol.Hover{
		Contents: protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: res.Contents},
		Range:    &rng,
	}, nil
}
