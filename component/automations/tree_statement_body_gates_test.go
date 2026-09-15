package automations_test

// tree_statement_body_gates_test.go -- the boot gates that read a statement
// body read every body of the shipped tree (epic memql#5370).

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslgate"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestTreePassesTheStatementBodyGates: the boot gates that read a statement
// body (dslgate: a config read outside the allow-list, a bare call naming
// nothing) find nothing in the shipped tree, and they read every body the
// whole-file parse finds -- so their silence is not a body they could not
// read.
func TestTreePassesTheStatementBodyGates(t *testing.T) {
	tree := memqldsl.Tree()

	// The corpus as boot hands it to the gates: each file through the front
	// end of its domain's edition, as dslgate.ScanTree reads it.
	lines, _ := languageParser.ResolveLanguageLines(tree, memqldsl.EmbeddedTree{})
	paths, err := dslfs.WalkMemqlFiles(tree)
	require.NoError(t, err)
	var files []dslgate.SourceFile
	var bodies []string
	for _, p := range paths {
		raw, err := fs.ReadFile(tree, p)
		require.NoError(t, err)
		prepared, err := lines.Prepare(p, raw)
		require.NoErrorf(t, err, "the front end refuses %s", p)
		files = append(files, dslgate.SourceFile{Path: p, Content: string(prepared)})
		if !bodyDeclaration.MatchString(languageParser.BlankCommentsAndStrings(string(prepared))) {
			continue
		}
		pf, err := languageParser.ParseFile(string(prepared))
		require.NoErrorf(t, err, "%s does not parse", p)
		for _, d := range pf.Definitions {
			if fn, ok := d.(*ast.FunctionDef); ok {
				if auto, ok := fn.Body.(*ast.AutomationDef); ok && auto.Body != nil {
					bodies = append(bodies, p+" "+fn.Name)
				}
			}
		}
	}
	require.Greater(t, len(bodies), shippedAutomationCount, "the tree holds fewer statement bodies than it has automations; the parse went blind")
	require.ElementsMatch(t, bodies, dslgate.StatementBodiesRead(files), "the gates read other bodies than the tree holds")

	var found []string
	for _, v := range dslgate.ScanFiles(files, dslgate.Options{}) {
		if v.Gate == dslgate.GateStatementConfigKey || v.Gate == dslgate.GateStatementUnknownCall {
			found = append(found, v.String())
		}
	}
	require.Emptyf(t, found, "the tree fails the statement-body gates:\n%s", strings.Join(found, "\n"))
	t.Logf("the statement-body gates read %d bodies of the tree and find nothing", len(bodies))
}

// bodyDeclaration finds a logic or automation declaration in a view with
// comments and strings blanked.
var bodyDeclaration = regexp.MustCompile(`(?m)^[ \t]*(logic|automation)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
