// Package anchor links the storefront packs, once, for everything that
// needs an engine carrying them.
//
// # Why this is a package and not a line in app/
//
// It was a line in app/, and that made the storefront packs reachable to a
// running node and to nothing else. The offline package analyzer
// (component/packages) boots a DSL tree to answer "would this package
// boot", and it linked no packs -- so a product domain that did the thing
// epic memql#5532 exists to enable, `use wholesale.concepts.{ application }`,
// was told its import resolved to nothing. The message even asked the
// author to add the import they had already written.
//
// The analyzer MODELS AN ENGINE, and every published image carries these
// packs with no build tag. An analyzer that cannot see them refuses exactly
// the packages the epic was for.
//
// # It is idempotent, and it has to be
//
// dsl.RegisterTree panics on a second registration of one namespace, which
// is right: a namespace registered twice is two trees disagreeing about one
// name. But a node ALSO analyzes packages in-process (the Deployables
// pipeline), so the anchor runs on two paths in one process. sync.Once is
// what lets both call it without either having to know about the other.
package anchor

import (
	"sync"

	"github.com/znasllc-io/memql/packs/reviewspack"
	"github.com/znasllc-io/memql/packs/wholesalepack"
)

var once sync.Once

// Storefront registers every storefront pack, once per process.
//
// REACH IS NOT DECIDED HERE. Linking a pack is not enabling it: a
// storefront pack ships DISABLED and its reach is governed by
// v1:platform:packState, which is read later and folded over the declared
// defaults. This function's whole job is that the concepts exist to be
// imported and related to.
func Storefront() {
	once.Do(func() {
		reviewspack.Register(reviewspack.Domain)
		wholesalepack.Register(wholesalepack.Domain)
	})
}

// Domains names what Storefront links, for a caller that reports it.
func Domains() []string {
	return []string{reviewspack.Domain, wholesalepack.Domain}
}
