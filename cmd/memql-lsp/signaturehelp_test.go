package main

import (
	"sort"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql/sense"
)

// TestSignatureHelpHandler_Builtin drives the handler over EVERY builtin
// carrying a signature, in sorted order.
//
// It used to pick one at random -- `for n := range sense.BuiltinFunctions {
// ... break }` over a map, which Go iterates in a randomised order -- and
// assert that the handler returned exactly that builtin's reading. The
// assertion is untrue for `contains`, which is also a relationship-traversal
// wrapper and so has two readings, so the test failed on roughly one CI run in
// thirty-four and passed on the rest (memql#3779). A random subject also meant
// thirty-three builtins were untested on any given run.
//
// Sorted and exhaustive fixes both halves: no randomness to flake on, and
// every builtin actually checked. The property asserted is the one that is
// true of all of them -- the builtin's own reading is AMONG the signatures
// offered -- rather than "is the only one", which the two-reading names break.
func TestSignatureHelpHandler_Builtin(t *testing.T) {
	names := make([]string, 0, len(sense.BuiltinFunctions))
	for name, def := range sense.BuiltinFunctions {
		if def.Signature != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no builtin carries a signature; the projection from dslspec is empty")
	}

	s := newTestServerWithSense(t, sense.New(nil))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			uri := protocol.DocumentUri("file:///" + name + ".memql")
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
			if sh == nil || len(sh.Signatures) == 0 {
				t.Fatalf("no signature offered for builtin %q", name)
			}

			want := sense.BuiltinFunctions[name].Signature
			var labels []string
			for _, sig := range sh.Signatures {
				if sig.Label == want {
					if sh.ActiveParameter == nil {
						t.Error("active parameter should be set")
					}
					return
				}
				labels = append(labels, sig.Label)
			}
			t.Errorf("the builtin's own reading %q is not among the signatures offered: %v", want, labels)
		})
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
