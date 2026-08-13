package memql

// graph_edge_labels_test.go is the memql#3657 drift pin for the graph edge
// vocabulary -- the strings a client reads off `result.bundle.edges[].type`.
//
// Three descriptions of that vocabulary used to exist and two were wrong,
// including the public docs a downstream repo codes against: memql.md
// documented `aliases` and `interactions` (actual: `alias`, `interactsWith`)
// and omitted `equals` entirely, so a client following our own reference
// never matched. The drift was possible because the values were spelled at
// seven call sites with nothing tying them to the documentation.
//
// These assertions close that loop. Each one names the exact drifted symbol so
// a future failure explains itself:
//
//  1. the GraphEdgeLabel* constants declared in the package are exactly the
//     ones this test knows about;
//  2. those constants' values are exactly GraphEdgeLabels();
//  3. every addEdge call site emits one of them, never a literal;
//  4. the table in the EMBEDDED public guide lists exactly GraphEdgeLabels().
//
// (4) reads docs.MemqlGuide() rather than the file on disk deliberately: that
// is the artifact shipped in the binary and served by the memqlGuide builtin,
// so the pin covers what a client actually receives.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/docs"
)

// edgeLabelIdentifiers maps each exported edge-label constant's NAME to its
// VALUE. It is deliberately a hand-written table: it is the pivot the other
// assertions turn on, and it is cross-checked in both directions -- against
// the constants actually declared in the package (so a new one cannot be
// missed) and against GraphEdgeLabels() (so a constant cannot exist outside
// the exported set). Adding an edge label means adding it here, which is the
// moment to decide whether it belongs on the wire and in the docs.
var edgeLabelIdentifiers = map[string]string{
	"GraphEdgeLabelChild":         GraphEdgeLabelChild,
	"GraphEdgeLabelAlias":         GraphEdgeLabelAlias,
	"GraphEdgeLabelEquals":        GraphEdgeLabelEquals,
	"GraphEdgeLabelInteractsWith": GraphEdgeLabelInteractsWith,
	"GraphEdgeLabelCreatedBy":     GraphEdgeLabelCreatedBy,
	"GraphEdgeLabelContains":      GraphEdgeLabelContains,
	"GraphEdgeLabelOwns":          GraphEdgeLabelOwns,
}

// edgeLabelDocsHeading anchors the authoritative table in the public guide.
const edgeLabelDocsHeading = "#### Edge labels on the wire"

// traversalFuncDocsHeading anchors the traversal-function table in the public
// guide.
const traversalFuncDocsHeading = "## Relationship Functions"

// TestGraphEdgeLabelConstantsAreComplete pins the declared GraphEdgeLabel*
// constants against edgeLabelIdentifiers and GraphEdgeLabels(). A new label
// constant that skips either lands here rather than silently reaching a client.
func TestGraphEdgeLabelConstantsAreComplete(t *testing.T) {
	declared := declaredEdgeLabelConstants(t)

	for name := range declared {
		if _, ok := edgeLabelIdentifiers[name]; !ok {
			t.Errorf("constant %s is declared in package memql but missing from "+
				"edgeLabelIdentifiers -- add it there, and to GraphEdgeLabels() and the "+
				"%q table in docs/public/language/memql.md if it is emitted on the wire",
				name, edgeLabelDocsHeading)
		}
	}
	for name := range edgeLabelIdentifiers {
		if !declared[name] {
			t.Errorf("edgeLabelIdentifiers names %s but no such constant is declared in "+
				"package memql -- remove it here, from GraphEdgeLabels(), and from the "+
				"%q table in docs/public/language/memql.md", name, edgeLabelDocsHeading)
		}
	}

	inSet := map[string]bool{}
	for _, label := range GraphEdgeLabels() {
		if inSet[label] {
			t.Errorf("GraphEdgeLabels() lists %q twice", label)
		}
		inSet[label] = true
	}
	for name, value := range edgeLabelIdentifiers {
		if !inSet[value] {
			t.Errorf("%s = %q is not in GraphEdgeLabels() -- an edge-label constant "+
				"outside the exported set cannot be documented or drift-checked", name, value)
		}
		delete(inSet, value)
	}
	for label := range inSet {
		t.Errorf("GraphEdgeLabels() contains %q, which no GraphEdgeLabel* constant "+
			"defines -- emit it from a named constant", label)
	}
}

// TestGraphEdgeLabelsAreEmittedFromTheConstantSet walks every addEdge call in
// the package's production files and requires its label argument to be one of
// the GraphEdgeLabel* identifiers. This is what stops a new edge kind from
// shipping as a bare string literal, which is how the vocabulary drifted from
// the docs in the first place.
func TestGraphEdgeLabelsAreEmittedFromTheConstantSet(t *testing.T) {
	fset := token.NewFileSet()
	sites := 0

	for _, file := range productionGoFiles(t, ".") {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "addEdge" {
				return true
			}
			sites++
			position := fset.Position(call.Pos())
			if len(call.Args) < 3 {
				t.Errorf("%s: addEdge called with %d args, expected at least 3",
					position, len(call.Args))
				return true
			}
			ident, ok := call.Args[2].(*ast.Ident)
			if !ok {
				t.Errorf("%s: addEdge label argument is not a plain identifier -- it must be "+
					"one of the GraphEdgeLabel* constants so the vocabulary stays checkable",
					position)
				return true
			}
			if _, known := edgeLabelIdentifiers[ident.Name]; !known {
				t.Errorf("%s: addEdge emits %s, which is not a GraphEdgeLabel* constant -- "+
					"the wire vocabulary is GraphEdgeLabels(), and it is what "+
					"docs/public/language/memql.md documents", position, ident.Name)
			}
			return true
		})
	}

	if sites == 0 {
		t.Fatal("found no addEdge call sites -- this pin has stopped covering the " +
			"edge emission path; find where GraphEdge.Type is now set")
	}
}

// TestGraphEdgeLabelsMatchPublicDocs is the client-facing half: the edge-label
// table in the embedded public guide must list exactly GraphEdgeLabels().
// Adding an edge label without documenting it, or documenting one the engine
// never emits, fails here.
func TestGraphEdgeLabelsMatchPublicDocs(t *testing.T) {
	documented := firstColumnCodeSpans(t, docs.MemqlGuide(), edgeLabelDocsHeading)

	if diff := diffStringSets(GraphEdgeLabels(), documented); diff != "" {
		t.Errorf("the %q table in docs/public/language/memql.md has drifted from "+
			"GraphEdgeLabels():\n%s", edgeLabelDocsHeading, diff)
	}
}

// TestTraversalFunctionsMatchPublicDocs pins the traversal-function table in
// the public guide against the RelationshipFunction constants the grammar
// actually accepts. Same failure shape as the edge labels: the guide is what a
// client repo writes its queries from.
func TestTraversalFunctionsMatchPublicDocs(t *testing.T) {
	documented := firstColumnCodeSpans(t, docs.MemqlGuide(), traversalFuncDocsHeading)
	for i, entry := range documented {
		documented[i] = strings.TrimSuffix(entry, "(expr)")
	}

	if diff := diffStringSets(declaredRelationshipFunctions(t), documented); diff != "" {
		t.Errorf("the %q table in docs/public/language/memql.md has drifted from the "+
			"RelationshipFunction constants in component/language/ast/ast.go:\n%s",
			traversalFuncDocsHeading, diff)
	}
}

// declaredEdgeLabelConstants returns the names of every GraphEdgeLabel*
// constant declared in the package's production files.
func declaredEdgeLabelConstants(t *testing.T) map[string]bool {
	t.Helper()

	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range productionGoFiles(t, ".") {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					if strings.HasPrefix(name.Name, "GraphEdgeLabel") {
						out[name.Name] = true
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no GraphEdgeLabel* constants -- the edge-label source of truth is gone")
	}
	return out
}

// declaredRelationshipFunctions returns the string values of the
// RelationshipFunction constants declared in the ast package. Read out of the
// source rather than restated so the ast package needs no exported list.
func declaredRelationshipFunctions(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "language", "ast", "ast.go")
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	out := make([]string, 0, 9)
	ast.Inspect(parsed, func(n ast.Node) bool {
		value, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		typeName, ok := value.Type.(*ast.Ident)
		if !ok || typeName.Name != "RelationshipFunction" {
			return true
		}
		for _, expr := range value.Values {
			literal, ok := expr.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			out = append(out, strings.Trim(literal.Value, `"`))
		}
		return true
	})

	if len(out) == 0 {
		t.Fatalf("found no RelationshipFunction constants in %s -- the traversal "+
			"vocabulary has moved and this pin needs re-aiming", path)
	}
	return out
}

// productionGoFiles lists the non-test .go files in dir.
func productionGoFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// firstColumnCodeSpans returns the backtick-quoted identifier in the first
// column of every body row of the first markdown table following heading.
func firstColumnCodeSpans(t *testing.T, guide, heading string) []string {
	t.Helper()

	lines := strings.Split(guide, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == heading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("heading %q not found in the embedded MemQL guide -- if it was renamed, "+
			"re-aim this pin at the new one rather than deleting it", heading)
	}

	out := make([]string, 0, 16)
	inTable := false
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			if inTable {
				break // table ended
			}
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		if len(cells) == 0 {
			continue
		}
		first := strings.TrimSpace(cells[0])
		if strings.Trim(first, "-: ") == "" {
			inTable = true // separator row
			continue
		}
		if !inTable {
			continue // header row, before the separator
		}
		span := strings.TrimSpace(first)
		if !strings.HasPrefix(span, "`") || !strings.HasSuffix(span, "`") {
			t.Errorf("row %q under %q does not start with a backtick-quoted identifier -- "+
				"this pin reads the first column as code spans", trimmed, heading)
			continue
		}
		out = append(out, strings.Trim(span, "`"))
	}

	if len(out) == 0 {
		t.Fatalf("no table rows found under %q in the embedded MemQL guide", heading)
	}
	return out
}

// diffStringSets returns a human-readable description of how documented
// differs from want, or "" when they hold the same values.
func diffStringSets(want, documented []string) string {
	wantSet := map[string]bool{}
	for _, value := range want {
		wantSet[value] = true
	}
	documentedSet := map[string]bool{}
	for _, value := range documented {
		documentedSet[value] = true
	}

	var missing, extra []string
	for value := range wantSet {
		if !documentedSet[value] {
			missing = append(missing, value)
		}
	}
	for value := range documentedSet {
		if !wantSet[value] {
			extra = append(extra, value)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	var report strings.Builder
	for _, value := range missing {
		report.WriteString("  emitted but not documented: " + value + "\n")
	}
	for _, value := range extra {
		report.WriteString("  documented but never emitted: " + value + "\n")
	}
	return report.String()
}
