package sense

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// #2629: the IsSnippet flag must survive the wire. Without it a
// consumer cannot distinguish snippet syntax from literal text and
// would insert the tabstops verbatim.
func TestCompletionItemSnippetWireRoundTrip(t *testing.T) {
	wire := []*memqlv1.SenseCompletionItem{
		{
			Label: "args { ... }", Kind: "snippet", Detail: "query block",
			Documentation: "Insert an args block.",
			InsertText:    "args {\n\t$0\n}", SortPriority: 1, IsSnippet: true,
		},
		{
			Label: "userId", Kind: "field", InsertText: "userId",
			SortPriority: 1, IsSnippet: false,
		},
	}

	var decoded []CompletionItem
	for _, item := range wire {
		decoded = append(decoded, CompletionItem{
			Label:         item.GetLabel(),
			Kind:          item.GetKind(),
			Detail:        item.GetDetail(),
			Documentation: item.GetDocumentation(),
			InsertText:    item.GetInsertText(),
			SortPriority:  int(item.GetSortPriority()),
			IsSnippet:     item.GetIsSnippet(),
		})
	}

	if !decoded[0].IsSnippet {
		t.Error("snippet flag lost decoding from the wire")
	}
	if decoded[0].InsertText != "args {\n\t$0\n}" {
		t.Errorf("snippet text mangled: %q", decoded[0].InsertText)
	}
	if decoded[1].IsSnippet {
		t.Error("plain item must not decode as a snippet")
	}
}
