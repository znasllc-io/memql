package main

import (
	"strings"
	"testing"

	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql/sense"
)

// TestPublishDiagnostics_V1RefusalSquigglesTheAuthorsToken: the squiggle an
// editor draws for a refusal inside a filter the rewriter moved into its
// synthesized return covers the token the author wrote -- in LSP's 0-based
// UTF-16 coordinates, past a non-ASCII string on the same line (memql#5364).
func TestPublishDiagnostics_V1RefusalSquigglesTheAuthorsToken(t *testing.T) {
	s := newTestServerWithSense(t, sense.New(nil))
	notify, got := capturingNotify()

	src := `use probe.concepts.{ thing }

query thing byA {
  args {
    a string @required
  }
  filter row => row.a == args.a && row.name == "名前" && row.b == null
  paginate 10
}
`
	const uri = "file:///probe/queries.memql"
	s.docs.open(uri, src)
	s.publishDiagnostics(notify, uri)

	if len(*got) != 1 || len((*got)[0].Diagnostics) != 1 {
		t.Fatalf("want one publish with one diagnostic, got %+v", *got)
	}
	d := (*got)[0].Diagnostics[0]
	line := src[strings.LastIndex(src[:strings.Index(src, "null")], "\n")+1:]
	line = line[:strings.Index(line, "\n")]
	wantLine := uint32(strings.Count(src[:strings.Index(src, "null")], "\n"))
	// UTF-16 code units before `null` on its line: every rune there is in the
	// BMP, so it is the rune count.
	wantChar := uint32(len([]rune(line[:strings.Index(line, "null")])))
	if d.Range.Start.Line != wantLine || d.Range.Start.Character != wantChar {
		t.Fatalf("squiggle starts at %d:%d, want %d:%d (the author's `null`): %s", d.Range.Start.Line, d.Range.Start.Character, wantLine, wantChar, d.Message)
	}
	if d.Range.End.Line != wantLine || d.Range.End.Character != wantChar+uint32(len("null")) {
		t.Fatalf("squiggle ends at %d:%d, want %d:%d: it must cover the token and nothing past it", d.Range.End.Line, d.Range.End.Character, wantLine, wantChar+uint32(len("null")))
	}
}

// TestPublishDiagnostics_RetiredFilterSquigglesItsClause: past the flip, a
// query's legacy filter is refused, and the squiggle -- where the lightbulb
// for "Rewrite to edition 2026" appears -- covers that clause's predicate on
// the filter's own line. It used to land one line low, on `paginate`, zero
// wide: the refusal sat on the rewriter's synthesized `concept==` join, and
// the line map could place that line but not a column.
func TestPublishDiagnostics_RetiredFilterSquigglesItsClause(t *testing.T) {
	expressionsV1(t)
	s := newTestServerWithSense(t, sense.New(nil))
	notify, got := capturingNotify()

	// memqlmigrate:keep -- the legacy filter is the case.
	src := `use probe.concepts.{ thing }

query thing openThings {
  args {
    status string
  }
  filter  status == args.status
  paginate 10
}
`
	const uri = "file:///probe/queries.memql"
	s.docs.open(uri, src)
	s.publishDiagnostics(notify, uri)
	if len(*got) != 1 {
		t.Fatalf("want one publish, got %d", len(*got))
	}
	var d *protocol.Diagnostic
	for i := range (*got)[0].Diagnostics {
		if diagnosticCode((*got)[0].Diagnostics[i]) == "retired_filter_without_lambda" {
			d = &(*got)[0].Diagnostics[i]
		}
	}
	if d == nil {
		t.Fatalf("no retired_filter_without_lambda diagnostic: %+v", (*got)[0].Diagnostics)
	}
	predicate := "status == args.status"
	off := strings.Index(src, predicate)
	wantLine := uint32(strings.Count(src[:off], "\n"))
	wantStart := uint32(off - (strings.LastIndex(src[:off], "\n") + 1))
	want := protocol.Range{
		Start: protocol.Position{Line: wantLine, Character: wantStart},
		End:   protocol.Position{Line: wantLine, Character: wantStart + uint32(len(predicate))},
	}
	if d.Range != want {
		t.Fatalf("squiggle %+v, want %+v: the filter's predicate on its own line", d.Range, want)
	}
}
