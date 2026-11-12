// Package storestats keeps the store statistics: the facts a store's sales
// figures are summed from, made from the store's mirrored orders, lines and
// refunds (docs/superpowers/specs/2026-11-11-store-statistics-design.md). It
// is a component: it reads this cluster's own mirror rows and writes this
// cluster's own tables, and calls no outside service.
//
// # Why a side table
//
// A figure is a sum of money by day over thousands of orders, each with its
// lines and refunds. The query language cannot sum by day, and a page that
// walked a store's mirror on every open would cost every version of every
// order the store ever wrote. So, as the email statistics do
// (component/emailstats), a walk records one fact per mirrored order, line and
// refund in tables of this package, and the read sums those with ordinary SQL.
// The money in a fact is exact (core/money): nothing here goes through a
// float, and the one amount computed by division, a line's share of its
// discounts, is rounded half to even at the larger of the scale Shopify wrote
// and the currency's minor units (facts.go, soldOf).
//
// # Why through the engine
//
// The mirror rows are graph rows, and hand-written SQL over the graph is the
// hazard the design rules out: the latest-version collapse, the id spellings
// and the row admission live in the engine. The walk reads the rows through
// the @serverOnly shapes of dsl/shopify/overlay/shapes.memql, each of which
// projects what a figure is made of and nothing more: no name, no address, no
// phone. The Go list of the concepts those shapes read is walkedConcepts, held
// equal to the shapes by a test that reads the DSL.
//
// # What a version is
//
// A fact's version is its mirror row's own write time, the row's createdAt:
// the instant the mirror wrote that version of the row. It is how the engine
// itself orders a row's versions, and it moves forward on every write, a
// tombstone and a revival included. The row's updatedAt cannot serve: it is
// Shopify's version only when versionSource is origin, and a tombstone carries
// the time MemQL read Shopify there. A fact is written only over an older
// version of itself, never over a newer one.
//
// # Why a retired row's fact is kept
//
// When the mirror retires a row it keeps a tombstone, the same row with
// deleted true and its other fields as they last were; a row is a tombstone by
// deleted alone. Its fact is not removed but marked deleted at the tombstone's
// version, because the version guard needs a row to compare with: without
// one, the older live version of a deleted order, which a later window,
// another node or the check reads again, would be written back as if the
// order were new. Deleted facts count nowhere.
//
// # What of a buyer a fact holds, and what an erasure does to it
//
// A fact holds no name, email, address or phone. What an order fact keeps of
// its buyer is the Shopify customer's gid (BuyerKey), and a line fact keeps
// the line's title and its product. The order's email is read in memory for
// the wholesale judgement and never written, logged or put in an error.
//
// A customer's erasure rewrites the mirror rows in place. Every field the
// facts are made from is one the erasure keeps (erasure_gate_test.go holds it
// against shopify.ErasureDecision), with two exceptions the check repairs: an
// erased order's email becomes the erasure's marker, so the order becomes
// retail; and a custom item, a line with no product, has its title scrubbed,
// so its fact's title and product key become the marker. The erasure keeps the
// order's customer reference while it empties the customer's own row, so the
// buyer key names nobody. The order's money stays in every figure.
//
// # Who reads the figures
//
// builtin storeStatistics (statistics.go) is the one read a person calls. It
// answers a store's figures to a holder of the sales part
// (app:deployables/sales) in the organization of a storefront site that
// serves the store, and it holds that gate in Go itself, whatever construct
// reached it, before anything of a store is read.
package storestats
