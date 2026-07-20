package main

import (
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// #2629: the LSP mapping must declare the insert-text format, or
// snippet syntax inserts LITERALLY (dollar signs in the buffer).
func TestToLSPCompletionItemInsertTextFormat(t *testing.T) {
	snip := toLSPCompletionItem(sense.CompletionItem{
		Label: "args { ... }", Kind: "snippet",
		InsertText: "args {\n\t$0\n}", IsSnippet: true,
	})
	if snip.InsertTextFormat == nil {
		t.Fatal("snippet item must declare InsertTextFormat")
	}
	if *snip.InsertTextFormat != protocol.InsertTextFormatSnippet {
		t.Errorf("snippet format = %v, want Snippet", *snip.InsertTextFormat)
	}

	plain := toLSPCompletionItem(sense.CompletionItem{
		Label: "userId", Kind: "field", InsertText: "userId",
	})
	if plain.InsertTextFormat == nil {
		t.Fatal("plain item must declare InsertTextFormat explicitly")
	}
	if *plain.InsertTextFormat != protocol.InsertTextFormatPlainText {
		t.Errorf("plain format = %v, want PlainText", *plain.InsertTextFormat)
	}
}
