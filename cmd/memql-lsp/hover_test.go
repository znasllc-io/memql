package main

import (
	"strings"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql"
)

func TestHoverHandler_Concept(t *testing.T) {
	svc, err := memql.BuildOfflineSense(nil)
	if err != nil {
		t.Fatalf("BuildOfflineSense: %v", err)
	}
	s := newTestServerWithSense(t, svc)
	const uri = "file:///t.memql"
	s.docs.open(uri, "ref v1:identity:user")

	h, err := s.hover(noopCtx(), &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     protocol.Position{Line: 0, Character: 10}, // on the concept id
		},
	})
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if h == nil {
		t.Fatal("expected a hover over a known concept id")
	}
	mc, ok := h.Contents.(protocol.MarkupContent)
	if !ok || mc.Kind != protocol.MarkupKindMarkdown {
		t.Fatalf("hover contents = %+v; want MarkupContent markdown", h.Contents)
	}
	if !strings.Contains(mc.Value, "v1:identity:user") {
		t.Errorf("hover should mention the concept; got %q", mc.Value)
	}
	if h.Range == nil {
		t.Error("hover should carry a range")
	}
}

func TestHoverHandler_UnknownDocument(t *testing.T) {
	svc, _ := memql.BuildOfflineSense(nil)
	s := newTestServerWithSense(t, svc)
	h, err := s.hover(noopCtx(), &protocol.HoverParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: "file:///missing.memql"},
			Position:     protocol.Position{Line: 0, Character: 0},
		},
	})
	if err != nil || h != nil {
		t.Errorf("unknown document should hover nil, got %+v err=%v", h, err)
	}
}
