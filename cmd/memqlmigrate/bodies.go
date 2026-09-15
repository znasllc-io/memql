package main

// bodies.go -- `memqlmigrate --rewrite=bodies` (epic memql#5370, task
// memql#5373): the bodies rewrite (component/language/bodymigrate) over a
// tree, its bare calls resolved over the tree AND the engine's embedded one,
// because a bundle calls core mutations and builtins it does not declare.

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/znasllc-io/memql/component/language/bodymigrate"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// rewriteBodies is the registry's tree rewrite.
func rewriteBodies(root string, files map[string][]byte) (map[string][]byte, error) {
	ix, err := buildDeclIndex(files)
	if err != nil {
		return nil, err
	}
	return bodymigrate.RewriteTree(files, ix)
}

// buildDeclIndex indexes files plus the engine's embedded tree.
func buildDeclIndex(files map[string][]byte) (*bodymigrate.Index, error) {
	ix := bodymigrate.IndexFiles(files)
	err := fs.WalkDir(memqldsl.Tree(), ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || path.Ext(p) != ".memql" || strings.HasPrefix(path.Base(path.Dir(p)), "_") {
			return nil
		}
		b, rerr := fs.ReadFile(memqldsl.Tree(), p)
		if rerr != nil {
			return rerr
		}
		ix.AddSource(string(b))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index the embedded tree: %w", err)
	}
	return ix, nil
}
