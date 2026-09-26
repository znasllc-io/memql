package app

import (
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/packs/anchor"
)

// anchor_storefront_packs.go links the STOREFRONT PACKS into the default
// build (epic memql#5532, issue memql#5549).
//
// # What this closes
//
// A pack's Go half "is compiled in via build tags ... there is no runtime
// loading of the Go half, by design" (docs/public/build/building-a-pack.md),
// and no published image sets a pack's tag: the images are built with
// BUILD_TAGS=<node type> and nothing else. examples/reviewspack registered
// under a tag of its own name, and the carrier route that once compiled a
// product's Go in is retiring (memql#2472). So no pack could reach a running
// cluster at all -- gap G6 of the design record.
//
// # NO BUILD TAG, and that is the decision
//
// Every node type mounts this. A pack's REACH is governed by
// v1:platform:packState now rather than by which binary happened to link it,
// and a tag here would be a second, invisible switch disagreeing with the
// first: an operator enabling reviews in Cluster > Modules would see it load
// on some nodes and not others, with the module inventory correctly
// reporting both.
//
// anchor_deploypack.go stays tagged for the opposite reason: the deploy pack
// is mounted on identity because that is where deployment records are
// written, which is a fact about where the work happens rather than a
// per-instance choice.
//
// # It must run BEFORE concept loading and loadPackEnablement
//
// RegisterPackDefault is what makes "absence of a row means the pack's
// declared default" mean anything, and the rows are folded over the
// declarations in phase 3. The database phase anchors before its first
// concept load, so product imports can also resolve pack concepts. Anchoring
// after the enablement read would leave the
// declaration unheard and reviews would ship ENABLED -- the exact outcome
// the default exists to prevent, arriving silently.
func (a *App) anchorStorefrontPacks() {
	// THE REGISTRATION ITSELF LIVES IN packs/anchor, and it is idempotent.
	// It has to be: the offline package analyzer needs an engine carrying
	// these packs too (a product's DSL imports their concepts), and a node
	// runs that analyzer IN-PROCESS for the Deployables pipeline. Two
	// callers, one process, and dsl.RegisterTree panics on a second
	// registration -- so both go through one sync.Once rather than either
	// having to know about the other.
	//
	// THE WHOLESALE PACK JOINS ON THE SAME TERMS (epic memql#5533). No build
	// tag, disabled by default, reach governed by packState -- and it is
	// anchored with reviews rather than in its own file for the reason this
	// file exists at all: a reader asking "which packs does a default image
	// carry?" must be able to answer it from one place.
	anchor.Storefront()
	if a != nil && a.Logger != nil {
		a.Logger.Info("storefront packs linked into this build; reach is governed by "+
			"v1:platform:packState",
			"component", memql.ComponentName,
			"packs", anchor.Domains(),
			"defaults", memqldsl.PackDefaults())
	}
}
