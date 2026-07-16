package main

import (
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
)

// signatureHelpTriggerChars fire signature help inside a call's argument list.
var signatureHelpTriggerChars = []string{"(", ","}

func (s *server) signatureHelp(_ *glsp.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	svc := s.getSense()
	if svc == nil {
		return nil, nil
	}
	line, col := position.FromLSPPosition(text, params.Position)
	res := svc.SignatureHelp(text, line, col)
	if res == nil {
		return nil, nil
	}

	sigs := make([]protocol.SignatureInformation, 0, len(res.Signatures))
	for _, sig := range res.Signatures {
		var params_ []protocol.ParameterInformation
		for _, p := range sig.Parameters {
			pi := protocol.ParameterInformation{Label: p.Label}
			if p.Documentation != "" {
				pi.Documentation = protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: p.Documentation}
			}
			params_ = append(params_, pi)
		}
		si := protocol.SignatureInformation{Label: sig.Label, Parameters: params_}
		if sig.Documentation != "" {
			si.Documentation = protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: sig.Documentation}
		}
		sigs = append(sigs, si)
	}

	activeSig := protocol.UInteger(res.ActiveSignature)
	activeParam := protocol.UInteger(res.ActiveParameter)
	return &protocol.SignatureHelp{
		Signatures:      sigs,
		ActiveSignature: &activeSig,
		ActiveParameter: &activeParam,
	}, nil
}
