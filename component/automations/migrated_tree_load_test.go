package automations

// migrated_tree_load_test.go -- the flip's acceptance for automations, run
// ahead of the flip (epic memql#5370, plan Task 13).
//
// The tree as `memqlmigrate --rewrite=bodies` carries it (the tree is in the
// edition-2026 expression grammar already) goes through the boot walk itself (LoadFromTree): the edition front end, extraction, compile, the
// trigger-wiring refusal and strict load. cmd/memqlmigrate's
// TestBodiesRewriteOfTheTreeIsTheLanguage checks that the same migration
// parses and passes the scope rules; this checks that the loader takes it,
// with every automation a statement body.

import (
	"io"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/bodymigrate"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslgate"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// migratedTree is the embedded tree with its .memql files carried by the
// bodies rewrite over the whole tree; every other file as it is. A
// soft-disabled directory (`_` or `.`) is copied untouched, as the rewrites
// and the loaders skip it.
func migratedTree(t *testing.T) fstest.MapFS {
	t.Helper()
	src := memqldsl.Tree()
	out := fstest.MapFS{}
	files := map[string][]byte{}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		out[p] = &fstest.MapFile{Data: b}
		if path.Ext(p) == ".memql" && !softDisabled(p) {
			files[p] = b
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, len(files), 50, "the embedded tree reads few .memql files; the walk went blind")

	changed, err := bodymigrate.RewriteTree(files, bodymigrate.IndexFiles(files))
	require.NoError(t, err, "the bodies rewrite refuses the tree")
	require.NotEmpty(t, changed, "the bodies rewrite changed nothing: the tree is already in statements, and this test has nothing left to check")
	for p, b := range files {
		out[p] = &fstest.MapFile{Data: b}
	}
	for p, b := range changed {
		// The rewrite renames a `mutate <Concept> <name> {` declaration to
		// `mutation`, a keyword the parser takes only from the flip on (plan
		// Task 13 step 6, with every other site that reads it). Until then the
		// header is read under the keyword the parser has; nothing here tests
		// the spelling. The flip deletes this line.
		b = mutationDeclaration.ReplaceAll(b, []byte("${1}mutate$2"))
		out[p] = &fstest.MapFile{Data: b}
	}
	return out
}

// mutationDeclaration is a mutation's declaration header in the keyword the
// flip introduces: at a line's start, two identifiers, then the brace.
var mutationDeclaration = regexp.MustCompile(`(?m)^([ \t]*)mutation([ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{)`)

// softDisabled reports a path under a `_` or `.` directory.
func softDisabled(p string) bool {
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if strings.HasPrefix(seg, "_") || (strings.HasPrefix(seg, ".") && seg != ".") {
			return true
		}
	}
	return false
}

// TestMigratedTreeAutomationsLoad: the migrated tree loads through the boot
// walk with no problem, the same automations the shipped tree loads, each one
// a statement body.
func TestMigratedTreeAutomationsLoad(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	shipped, err := NewLoader(LoaderOptions{Logger: logger}).LoadFromUnifiedTree()
	require.NoError(t, err)

	tree := migratedTree(t)

	migrated, err := NewLoader(LoaderOptions{Logger: logger}).LoadFromTree(tree)
	require.NoError(t, err, "the migrated tree does not load")

	names := func(as []*Automation) []string {
		out := make([]string, 0, len(as))
		for _, a := range as {
			out = append(out, a.Name)
		}
		return out
	}
	require.ElementsMatch(t, names(shipped), names(migrated), "the migrated tree loads other automations than the shipped one")
	var legacy []string
	for _, a := range migrated {
		if !a.IsStatementBody() {
			legacy = append(legacy, a.Name)
		}
	}
	require.Emptyf(t, legacy, "%d automations did not load as statement bodies", len(legacy))
	t.Logf("%d automations load from the migrated tree, every one a statement body", len(migrated))
}

// TestMigratedTreeImportsResolve: the migrated tree's imports and construct
// references resolve exactly as the shipped tree's do. The rewrite moves a
// publishing logic into its automation and deletes it, so an import of it
// left anywhere would dangle -- a refusal only the whole-tree integrity lanes
// (dslimports, run by engine Init and memqllint) see.
func TestMigratedTreeImportsResolve(t *testing.T) {
	findings := func(root fs.FS) []string {
		t.Helper()
		tree, err := dslimports.Load(root)
		require.NoError(t, err)
		var out []string
		for _, e := range tree.VerifyReferentialIntegrity() {
			out = append(out, e.Error())
		}
		return out
	}
	shipped := findings(memqldsl.Tree())

	tree := migratedTree(t)
	require.ElementsMatch(t, shipped, findings(tree), "the migrated tree's references resolve otherwise than the shipped tree's")
}

// TestMigratedTreePassesTheStatementBodyGates: the boot gates that read a
// statement body (dslgate: a config read outside the allow-list, a bare call
// naming nothing) find nothing in the migrated tree. Until the flip they pass
// over every shipped body, each in a retired form, so this is the first tree
// they read -- and they read every body the whole-file parse finds, so their
// silence is not a body they could not read.
func TestMigratedTreePassesTheStatementBodyGates(t *testing.T) {
	tree := migratedTree(t)

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
		require.NoErrorf(t, err, "the migrated %s does not parse", p)
		for _, d := range pf.Definitions {
			if fn, ok := d.(*ast.FunctionDef); ok {
				if auto, ok := fn.Body.(*ast.AutomationDef); ok && auto.Body != nil {
					bodies = append(bodies, p+" "+fn.Name)
				}
			}
		}
	}
	require.Greater(t, len(bodies), 58, "the migrated tree holds fewer statement bodies than it has automations; the parse went blind")
	require.ElementsMatch(t, bodies, dslgate.StatementBodiesRead(files), "the gates read other bodies than the migrated tree holds")

	var found []string
	for _, v := range dslgate.ScanFiles(files, dslgate.Options{}) {
		if v.Gate == dslgate.GateStatementConfigKey || v.Gate == dslgate.GateStatementUnknownCall {
			found = append(found, v.String())
		}
	}
	require.Emptyf(t, found, "the migrated tree fails the statement-body gates:\n%s", strings.Join(found, "\n"))
	t.Logf("the statement-body gates read %d bodies of the migrated tree and find nothing", len(bodies))
}

// bodyDeclaration finds a logic or automation declaration in a view with
// comments and strings blanked.
var bodyDeclaration = regexp.MustCompile(`(?m)^[ \t]*(logic|automation)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
