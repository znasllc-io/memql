package memql

// v1_refusal_position_test.go -- the engine's loaders report a refusal inside
// a construct at the author's line and column in the FILE (memql#5364). Each
// loader parses a slice of the file, lowered, so a position it printed was the
// lowered slice's: line 2 for a spec whose doc comment is line 1, and the
// synthesized return's column for a filter's token.

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// fileAt is "line L, column C" of the `null` ending the comparison cmp in src.
func fileAt(src, cmp string) (line, col int, where string) {
	off := strings.Index(src, cmp) + len(cmp) - len("null")
	lineStart := strings.LastIndex(src[:off], "\n") + 1
	line, col = 1+strings.Count(src[:off], "\n"), 1+utf8.RuneCountInString(src[lineStart:off])
	return line, col, "at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(col) + ":"
}

const positionQueries = `use demo.concepts.{ item }

@enabled
@description("A clean query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}

@enabled
@description("Items missing a status, written with the retired null.")
query item unstatusedItems {
  args {
    name  string  @required
  }
  filter row => row.name == args.name && row.status == null
  paginate 10
}`

// TestFunctionSliceRefusalNamesTheFileLine: the unified function loader's
// slice of the SECOND query in a file reports the file's line and column.
func TestFunctionSliceRefusalNamesTheFileLine(t *testing.T) {
	var target *FunctionSlice
	for _, s := range ExtractFunctionSlices(positionQueries) {
		if s.Name == "unstatusedItems" {
			s := s
			target = &s
		}
	}
	if target == nil {
		t.Fatal("no slice for unstatusedItems")
	}
	_, err := dispatchPerConstructParser(*target, "unified:demo/queries.memql", memorynodes.DefaultRegistry())
	if err == nil {
		t.Fatal("a filter holding the retired null loaded")
	}
	if _, _, where := fileAt(positionQueries, "row.status == null"); !strings.Contains(err.Error(), where) {
		t.Fatalf("got %v, want the refusal %s", err, where)
	}
}

// TestSpecSliceRefusalNamesTheFileLine: the spec loader's slice -- a spec
// below another, with a doc comment -- reports the file's line.
func TestSpecSliceRefusalNamesTheFileLine(t *testing.T) {
	specs := `use demo.concepts.{ item }

@enabled
@description("An item with a name.")
spec item isNamed {
  return name != ""
}

/// An item with a status, written with the retired null.
spec item hasStatus = row => row.status != null`
	for _, s := range anchoredExtractAdapter(specs, "spec") {
		if s.Name != "hasStatus" {
			continue
		}
		_, err := languageParser.ParseSpecDecl(s.Source)
		if err == nil {
			t.Fatal("a spec holding the retired null parsed")
		}
		if _, _, where := fileAt(specs, "row.status != null"); !strings.Contains(err.Error(), where) {
			t.Fatalf("got %v, want the refusal %s", err, where)
		}
		return
	}
	t.Fatal("no slice for hasStatus")
}

// TestSandboxRefusalNamesTheBundlePosition: the authoring sandbox's diagnostic
// for a refusal inside a filter is the offending token in the bundle, column
// and end included -- the line map it used to fall back to could name the
// line of a filter the lowering moved, never the column.
func TestSandboxRefusalNamesTheBundlePosition(t *testing.T) {
	bundle := `@namespace("alpha")
concept widget {
  status string
}

@description("first")
@public
query widget allA {
  filter  status == "x"
}

@description("second")
@public
query widget unstatused {
  filter row => row.status == null
  paginate 10
}
`
	rep := SandboxCompileBundle(SplitBundleSource(bundle))
	for _, d := range rep.Diagnostics {
		if d.Name != "unstatused" {
			continue
		}
		if d.OK {
			t.Fatal("a filter holding the retired null compiled")
		}
		line, col, _ := fileAt(bundle, "row.status == null")
		if d.Line != line || d.Column != col || d.EndLine != line || d.EndColumn != col+len("null") {
			t.Fatalf("diagnostic at %d:%d-%d:%d, want %d:%d-%d:%d (the author's `null`): %s", d.Line, d.Column, d.EndLine, d.EndColumn, line, col, line, col+len("null"), d.Error)
		}
		return
	}
	t.Fatalf("no diagnostic for unstatused: %+v", rep.Diagnostics)
}
