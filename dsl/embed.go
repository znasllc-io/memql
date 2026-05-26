// Package dsl is the unified embed surface for the new domain-first
// .memql tree. It lives alongside the legacy per-kind embed packages
// (dsl/v1/concepts/, dsl/v1/queries/, etc.) during the transitional
// state of the file-restructure migration (see
// docs/dsl-import-model-refactor.md).
//
// The Tree fs.FS exposed here is what the new unified loader
// (component/memql/unified_loader.go) walks. Each per-kind legacy
// embed package continues to expose its own fs.FS for the legacy
// loaders until Pass 2 of the restructure migration retires them.
package dsl

import (
	"embed"
	"io/fs"
)

// embedFS holds every .memql + .tmpl under the domain-first tree.
// The all: prefix ensures _-prefixed files are included so the
// walker can apply the soft-disable rule consistently (whereas the
// default behavior would skip them at the embed step entirely).
//
//go:embed all:agents all:cluster all:cognition all:common all:curriculum all:data all:identity all:knowledge all:memql all:observability all:planner all:platform all:policies all:providers all:router all:safety all:workbench all:worker
var embedFS embed.FS

// Tree returns an fs.FS rooted at the unified DSL tree. Each top-
// level entry under the FS is a domain folder (cognition/,
// copresent/, ..., common/, providers/, policies/).
func Tree() fs.FS {
	return embedFS
}
