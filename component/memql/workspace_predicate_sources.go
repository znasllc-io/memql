package memql

// workspace_predicate_sources.go -- the sources the expressions codemod reads
// its predicate set from, for a file in an editor workspace (memql#5364).
//
// The rewrite of a filter clause turns a bare predicate name into
// `name(row)` or `name(actor)`, and which of the two is decided by a spec
// declared in some other file, over a shape declared in yet another
// (langparser.CollectPredicates). memqlmigrate answers it by reading the whole
// tree it is pointed at; the language server's "Rewrite to edition 2026" quick
// fix must answer it the same way, from the same files, or the one-click fix
// and the CLI would write different text for one clause.

import (
	"io/fs"
	"path"
	"strings"

	"github.com/znasllc-io/memql/core/repowalk"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// WorkspacePredicateSources returns the .memql sources the expressions
// rewrite resolves a workspace file's predicates against, keyed by path
// relative to the workspace's DSL tree, and that tree's path relative to root
// ("" when root is the tree itself).
//
// The workspace half is what memqlmigrate --rewrite=expressions reads when it
// is pointed at the workspace's DSL tree (the tree BuildOfflineSense mounts):
// every regular .memql file under it. The core half is the embedded tree's
// files for every domain the workspace does not carry, because that is what
// the engine's flat predicate registry holds when the workspace loads over the
// core tree -- a bundle's filter naming a core trait (isActiveRecord) resolves
// to it. A workspace that carries a core domain (the engine repository's own
// dsl/) answers for that domain, as it does for BuildOfflineSense. A nil root
// yields the core tree alone.
func WorkspacePredicateSources(root fs.FS) (map[string][]byte, string) {
	files := map[string][]byte{}
	carried := map[string]bool{}
	tree, dir := resolveDSLRoot(root)
	if tree != nil {
		readMemqlTree(tree, func(p string) bool { return true }, func(p string, data []byte) {
			files[p] = data
			if i := strings.IndexByte(p, '/'); i > 0 {
				carried[p[:i]] = true
			}
		})
	}
	readMemqlTree(memqldsl.Tree(), func(p string) bool { return !carried[p] }, func(p string, data []byte) {
		if i := strings.IndexByte(p, '/'); i > 0 && carried[p[:i]] {
			return
		}
		files[p] = data
	})
	return files, dir
}

// readMemqlTree hands every regular .memql file under tree to add, descending
// only into the top-level directories enter accepts and never into a
// directory the repository walkers all skip. An unreadable entry is skipped:
// the result feeds an editor convenience, and a missing file costs a refusal
// the CLI would name, never a wrong rewrite.
func readMemqlTree(tree fs.FS, enter func(top string) bool, add func(p string, data []byte)) {
	_ = fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == "." {
				return nil
			}
			if repowalk.SkipDir(d.Name()) || (!strings.Contains(p, "/") && !enter(p)) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".memql" || !d.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(tree, p)
		if err != nil {
			return nil
		}
		add(p, data)
		return nil
	})
}
