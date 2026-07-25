package main

import (
	"path/filepath"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
)

// definition answers textDocument/definition (memql#2754): jump from a
// construct reference to its declaration.
//
// Sense reports the target as a path relative to the server root plus a
// line/column in that file's RAW source. The range needs no re-encoding
// through the open-buffer helper the way an in-document range does -- the
// target file is usually not open, and the position already refers to the
// on-disk text.
func (s *server) definition(_ *glsp.Context, params *protocol.DefinitionParams) (any, error) {
	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	svc := s.getSense()
	if svc == nil {
		return nil, nil
	}
	line, col := position.FromLSPPosition(text, params.Position)
	results := svc.Definition(text, line, col, uriToPath(params.TextDocument.URI))
	if len(results) == 0 {
		return nil, nil
	}
	locations := make([]protocol.Location, 0, len(results))
	for _, r := range results {
		locations = append(locations, protocol.Location{
			URI: pathToURI(filepath.Join(s.root, filepath.FromSlash(r.File))),
			Range: protocol.Range{
				Start: protocol.Position{
					Line:      uint32(max(r.Range.Start.Line-1, 0)),
					Character: uint32(max(r.Range.Start.Column-1, 0)),
				},
				End: protocol.Position{
					Line:      uint32(max(r.Range.End.Line-1, 0)),
					Character: uint32(max(r.Range.End.Column-1, 0)),
				},
			},
		})
	}
	return locations, nil
}
