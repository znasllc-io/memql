package memql

// workspace_predicate_sources.go -- the sources the expressions codemod reads
// its predicate set from, for a file in an editor workspace (memql#5364).
//
// The rewrite of a filter clause turns a bare predicate name into
// `name(row)` or `name(actor)`, and which of the two is decided by a spec
// declared in some other file, over a shape declared in yet another
// (langparser.CollectPredicates). memqlmigrate answers it by reading the whole
// tree it is pointed at, over the core tree it loads over; the language
// server's "Rewrite to edition 2026" quick fix must answer it the same way,
// from the same files, or the one-click fix and the CLI would write different
// text for one clause.

import (
	"io/fs"
	"path"

	"github.com/znasllc-io/memql/core/repowalk"
)

// WorkspacePredicateSources returns the .memql sources of the workspace's DSL
// tree -- the tree BuildOfflineSense mounts -- keyed by path relative to that
// tree, and the tree's path relative to root ("" when root is the tree
// itself). It is what memqlmigrate --rewrite=expressions reads when it is
// pointed at that tree: every regular .memql file under it. The caller
// resolves them over the core tree (langparser.ResolvePredicatesOver), as the
// CLI does. A nil root yields nothing.
func WorkspacePredicateSources(root fs.FS) (map[string][]byte, string) {
	files := map[string][]byte{}
	tree, dir := resolveDSLRoot(root)
	if tree == nil {
		return files, dir
	}
	_ = fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is skipped: the result feeds an editor
			// convenience, and a missing file costs a refusal the CLI would
			// name, never a wrong rewrite.
			if d != nil && d.IsDir() && p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != "." && repowalk.SkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".memql" || !d.Type().IsRegular() {
			return nil
		}
		if data, err := fs.ReadFile(tree, p); err == nil {
			files[p] = data
		}
		return nil
	})
	return files, dir
}
