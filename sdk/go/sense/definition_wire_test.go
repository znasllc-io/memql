package sense

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// #2760: go-to-definition over gRPC. The target carries a
// WORKSPACE-RELATIVE path rather than a URI, because the two consumers
// address files differently -- an LSP maps it to file://, a pack browser to
// (domain, path). A URI baked into the wire would serve only one of them.
func TestDefinitionTargetWireRoundTrip(t *testing.T) {
	wire := []*memqlv1.SenseDefinitionTarget{
		{
			File: "dsl/calendar/concepts.memql",
			Kind: "concept",
			Range: &memqlv1.SenseRange{
				Start: &memqlv1.SensePosition{Line: 24, Column: 9},
				End:   &memqlv1.SensePosition{Line: 24, Column: 22},
			},
		},
	}

	decoded := make([]DefinitionTarget, 0, len(wire))
	for _, t := range wire {
		decoded = append(decoded, DefinitionTarget{
			File:  t.GetFile(),
			Kind:  t.GetKind(),
			Range: protoRange(t.GetRange()),
		})
	}

	if len(decoded) != 1 {
		t.Fatalf("want 1 target, got %d", len(decoded))
	}
	got := decoded[0]
	if got.File != "dsl/calendar/concepts.memql" {
		t.Errorf("File = %q, want the workspace-relative path", got.File)
	}
	if got.Kind != "concept" {
		t.Errorf("Kind = %q, want concept -- the kind tells a consumer what it jumped to", got.Kind)
	}
	if got.Range.Start.Line != 24 || got.Range.Start.Column != 9 {
		t.Errorf("Start = %+v, want line 24 column 9", got.Range.Start)
	}
	if got.Range.End.Column != 22 {
		t.Errorf("End.Column = %d, want 22 -- the range must cover the declared name", got.Range.End.Column)
	}
}

// TestHoverMsgCarriesFilePath pins the field that closes the Cockpit gap:
// without it a bare concept name whose trailing segment collides across
// namespaces stays unresolved over gRPC, so Cockpit got no hover at all on
// those names while the LSP did.
func TestHoverMsgCarriesFilePath(t *testing.T) {
	msg := &memqlv1.SenseHoverMsg{
		Source:   "shape candidate candidateFull {\n}",
		Position: &memqlv1.SensePosition{Line: 1, Column: 10},
		FilePath: "dsl/actions/shapes.memql",
	}
	if msg.GetFilePath() != "dsl/actions/shapes.memql" {
		t.Errorf("FilePath = %q; the ambient domain rides on this field", msg.GetFilePath())
	}
}
