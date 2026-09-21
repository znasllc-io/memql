package main

// bodies_parse_test.go -- what the bodies rewrite writes is the language
// (epic memql#5370, task memql#5373).
//
// The goldens say what the rewrite writes; these tests say that what it
// writes is edition 2026. Each takes a tree the way a bundle repository
// migrates one -- `--rewrite=expressions,bodies` -- and hands every logic and
// automation of the result to the engine's own statement parser and scope
// checker. A construct the parser refuses, or a body the checker refuses, is
// a rewrite that produced something the engine will not load.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/compiler"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// migrateTree runs the two edition-2026 rewrites a bundle repository runs, in
// the documented order, and returns the whole tree after them.
func migrateTree(t *testing.T, files map[string][]byte) map[string][]byte {
	t.Helper()
	tree := map[string][]byte{}
	for p, b := range files {
		tree[p] = b
	}
	for _, step := range []struct {
		name string
		run  func(string, map[string][]byte) (map[string][]byte, error)
	}{{"expressions", rewriteExpressions}, {"bodies", rewriteBodies}} {
		changed, err := step.run("tree", tree)
		if err != nil {
			t.Fatalf("--rewrite=%s: %v", step.name, err)
		}
		for p, b := range changed {
			tree[p] = b
		}
	}
	return tree
}

// statementHeader finds the header of a logic or an automation.
var statementHeader = regexp.MustCompile(`(?m)^(logic|automation)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

// checkStatementBodies parses every logic and automation in one file alone --
// its annotation and doc-comment lines, its header and its braces -- and
// checks its body. It returns how many it checked.
func checkStatementBodies(t *testing.T, file string, src []byte) int {
	t.Helper()
	text := string(src)
	view := langparser.BlankCommentsAndStrings(text)
	checked := 0
	for _, m := range statementHeader.FindAllStringSubmatchIndex(view, -1) {
		kind, name := text[m[2]:m[3]], text[m[4]:m[5]]
		open := m[1] - 1
		closeAt := closingBrace(view, open)
		if closeAt < 0 {
			t.Errorf("%s: %s %s: no closing brace", file, kind, name)
			continue
		}
		piece := text[preambleStart(text, m[0]) : closeAt+1]
		norm, err := langparser.NormaliseAll(piece)
		if err != nil {
			t.Errorf("%s: %s %s: the rewriter refused the result: %v\n%s", file, kind, name, err, piece)
			continue
		}
		pf, err := langparser.ParseFile(norm)
		if err != nil {
			t.Errorf("%s: %s %s: the statement parser refused the result: %v\n%s", file, kind, name, err, piece)
			continue
		}
		for _, d := range pf.Definitions {
			fn, ok := d.(*langparser.FunctionDef)
			if !ok || fn.Name != name {
				continue
			}
			auto, ok := fn.Body.(*langparser.AutomationDef)
			if !ok || auto.Body == nil {
				t.Errorf("%s: %s %s: the result is still in a retired form", file, kind, name)
				continue
			}
			var args []string
			if fn.ArgsSchema != nil {
				for _, f := range fn.ArgsSchema.Fields {
					args = append(args, f.Name)
				}
			}
			for _, p := range compiler.CheckBody(kind, name, args, auto.Body) {
				t.Errorf("%s: %v", file, p)
			}
			checked++
		}
	}
	return checked
}

// closingBrace is the index of the `}` closing the `{` at open in a view with
// comments and strings blanked, or -1.
func closingBrace(view string, open int) int {
	depth := 0
	for i := open; i < len(view); i++ {
		switch view[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// preambleStart walks back over the annotation, doc-comment and comment lines
// directly above a header.
func preambleStart(src string, header int) int {
	start := header
	for start > 0 {
		lineStart := strings.LastIndexByte(src[:start-1], '\n') + 1
		line := strings.TrimSpace(src[lineStart : start-1])
		if !strings.HasPrefix(line, "@") && !strings.HasPrefix(line, "//") {
			break
		}
		start = lineStart
	}
	return start
}

func TestBodiesOutputIsTheLanguage(t *testing.T) {
	for _, name := range goldenCases(t) {
		name := name
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(bodiesTestdata, name)
			if _, err := os.Stat(filepath.Join(dir, "error.txt")); err == nil {
				t.Skip("a refusal case writes nothing")
			}
			tree := migrateTree(t, readGoldenTree(t, filepath.Join(dir, "in"), ".in"))
			checked := 0
			for _, p := range sortedTreePaths(tree) {
				checked += checkStatementBodies(t, p, tree[p])
			}
			if checked == 0 {
				t.Fatalf("the case holds no logic or automation after the rewrite: it checks nothing")
			}
		})
	}
}

// TestBodiesRewriteOfTheTreeIsTheLanguage migrates this repository's own
// trees in memory and checks every body. It is the flip's acceptance run
// ahead of the flip: when it passes, the tree migration will load.
func TestBodiesRewriteOfTheTreeIsTheLanguage(t *testing.T) {
	roots := []string{"../../dsl", "../../examples/deploypack/dsl", "../../examples/referencepack/dsl",
		"../../packs/reviewspack/dsl", "../../examples/shopifypack/dsl", "../../deploy/fleet/dsl"}
	checked := 0
	for _, root := range roots {
		files := map[string][]byte{}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".memql") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, p)
			files[filepath.ToSlash(rel)] = b
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tree := migrateTree(t, files)
		for _, p := range sortedTreePaths(tree) {
			if strings.Contains(p, "/_") || strings.HasPrefix(p, "_") {
				continue
			}
			checked += checkStatementBodies(t, fmt.Sprintf("%s/%s", root, p), tree[p])
		}
	}
	if checked < 100 {
		t.Fatalf("checked %d bodies; the walk is not reaching the tree", checked)
	}
}

func sortedTreePaths(tree map[string][]byte) []string {
	out := make([]string, 0, len(tree))
	for p := range tree {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
