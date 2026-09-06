// Part of the memql module split (memql#3228). The Materializer's PURE
// half (design record
// docs/superpowers/specs/2026-09-05-compose-materializer-design.md,
// epic memql#4977, section E): the format writers and the provenance
// stamps they embed.
//
// Everything here is a function from a composed draft plus a provenance
// record to BYTES -- no engine, no database, no provider, no blob
// storage. That is what lets the epic's headline claim about provenance
// -- every format either carries its own or says plainly that it cannot
// -- be a property of a function over values, checkable without a
// cluster, rather than a count against a mock. The wiring that reaches
// the Library lives in integrations/compose.
module github.com/znasllc-io/memql/component/compose

go 1.26.1

toolchain go1.26.6

require github.com/go-pdf/fpdf v0.9.0
