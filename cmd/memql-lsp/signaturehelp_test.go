package main

import (
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql/sense"
)

func TestSignatureHelpHandler_Builtin(t *testing.T) {
	// Pick any builtin with a signature so the handler's builtin branch runs.
	var name string
	for n, def := range sense.BuiltinFunctions {
		if def.Signature != "" {
			name = n
			break
		}
	}
	if name == "" {
		t.Skip("no builtin with a signature")
	}

	s := newTestServerWithSense(t, sense.New(nil))
	const uri = "file:///t.memql"
	line3 := "    return " + name + "("
	s.docs.open(uri, "logic x {\n  body {\n"+line3+"\n  }\n}")

	sh, err := s.signatureHelp(noopCtx(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     protocol.Position{Line: 2, Character: protocol.UInteger(len(line3))},
		},
	})
	if err != nil {
		t.Fatalf("signatureHelp: %v", err)
	}
	if sh == nil || len(sh.Signatures) != 1 {
		t.Fatalf("expected 1 signature for builtin %q, got %+v", name, sh)
	}
	if sh.Signatures[0].Label != sense.BuiltinFunctions[name].Signature {
		t.Errorf("signature label = %q; want %q", sh.Signatures[0].Label, sense.BuiltinFunctions[name].Signature)
	}
	if sh.ActiveParameter == nil {
		t.Error("active parameter should be set")
	}
}

func TestSignatureHelpHandler_NoneOutsideCall(t *testing.T) {
	s := newTestServerWithSense(t, sense.New(nil))
	const uri = "file:///t.memql"
	s.docs.open(uri, "logic x {\n  body {\n    return 1\n  }\n}")
	sh, err := s.signatureHelp(noopCtx(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position:     protocol.Position{Line: 2, Character: 5},
		},
	})
	if err != nil || sh != nil {
		t.Errorf("expected no signature outside a call, got %+v err=%v", sh, err)
	}
}
