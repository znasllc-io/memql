package main

import (
	"fmt"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// completionTriggerChars fire completion mid-token: annotations (@), member
// access (.), block open ({), and whitespace.
var completionTriggerChars = []string{"@", ".", "{", " "}

func (s *server) completion(_ *glsp.Context, params *protocol.CompletionParams) (any, error) {
	text, ok := s.docs.get(params.TextDocument.URI)
	if !ok {
		return nil, nil
	}
	svc := s.getSense()
	if svc == nil {
		return nil, nil
	}
	line, col := position.FromLSPPosition(text, params.Position)
	items := svc.Complete(text, line, col, uriToPath(params.TextDocument.URI))
	out := make([]protocol.CompletionItem, 0, len(items))
	for _, it := range items {
		out = append(out, toLSPCompletionItem(it))
	}
	return out, nil
}

func toLSPCompletionItem(it sense.CompletionItem) protocol.CompletionItem {
	kind := completionKind(it.Kind)
	ci := protocol.CompletionItem{Label: it.Label, Kind: &kind}
	if it.Detail != "" {
		detail := it.Detail
		ci.Detail = &detail
	}
	if it.Documentation != "" {
		ci.Documentation = protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: it.Documentation}
	}
	if it.InsertText != "" {
		insert := it.InsertText
		ci.InsertText = &insert
		// Snippet items must declare the format or LSP inserts the
		// tabstop syntax LITERALLY (#2629).
		if it.IsSnippet {
			format := protocol.InsertTextFormatSnippet
			ci.InsertTextFormat = &format
		} else {
			format := protocol.InsertTextFormatPlainText
			ci.InsertTextFormat = &format
		}
	}
	// LSP sorts sortText lexically; zero-pad the numeric priority so lower
	// priority (higher rank) sorts first.
	sortText := fmt.Sprintf("%08d", it.SortPriority)
	ci.SortText = &sortText
	return ci
}

var senseKindToLSP = map[string]protocol.CompletionItemKind{
	"keyword":    protocol.CompletionItemKindKeyword,
	"function":   protocol.CompletionItemKindFunction,
	"concept":    protocol.CompletionItemKindClass,
	"annotation": protocol.CompletionItemKindProperty,
	"builtin":    protocol.CompletionItemKindFunction,
	"spec":       protocol.CompletionItemKindFunction,
	"shape":      protocol.CompletionItemKindStruct,
	"provider":   protocol.CompletionItemKindValue,
	"tool":       protocol.CompletionItemKindFunction,
	"field":      protocol.CompletionItemKindField,
	"snippet":    protocol.CompletionItemKindSnippet,
	"receiver":   protocol.CompletionItemKindClass,
	"variable":   protocol.CompletionItemKindVariable,
	"namespace":  protocol.CompletionItemKindModule,
	"module":     protocol.CompletionItemKindModule,
}

func completionKind(k string) protocol.CompletionItemKind {
	if ck, ok := senseKindToLSP[k]; ok {
		return ck
	}
	return protocol.CompletionItemKindText
}
