// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
//
// The work spine's journal for server-STARTED passes (epic memql#4970): a
// goal, one run per attempt, one step per stage. A near-leaf, and it has to
// stay one -- its whole reason for existing as a package rather than a few
// calls is that it is small enough for the internal-origin allowlist to admit
// (component/auth/call_origin.go), and a package that grew a dependency on
// the engine would stop being that.
module github.com/znasllc-io/memql/component/workjournal

go 1.26.1

toolchain go1.26.6

require (
	github.com/znasllc-io/memql/component/auth v0.0.0
	github.com/znasllc-io/memql/component/language v0.0.0
)

require (
	github.com/znasllc-io/memql/component/language/annotations v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/ast v0.0.0 // indirect
	github.com/znasllc-io/memql/core v0.0.0 // indirect
)

replace github.com/znasllc-io/memql/component/actions => ../actions

replace github.com/znasllc-io/memql/component/auth => ../auth

replace github.com/znasllc-io/memql/component/bus => ../bus

replace github.com/znasllc-io/memql/component/bus/gen => ../bus/gen

replace github.com/znasllc-io/memql/component/config => ../config

replace github.com/znasllc-io/memql/component/database => ../database

replace github.com/znasllc-io/memql/component/events => ../events

replace github.com/znasllc-io/memql/component/envregistry => ../envregistry

replace github.com/znasllc-io/memql/component/grpc/gen => ../grpc/gen

replace github.com/znasllc-io/memql/component/harness => ../harness

replace github.com/znasllc-io/memql/component/identity => ../identity

replace github.com/znasllc-io/memql/component/language => ../language

replace github.com/znasllc-io/memql/component/language/annotations => ../language/annotations

replace github.com/znasllc-io/memql/component/language/ast => ../language/ast

replace github.com/znasllc-io/memql/component/language/dslclause => ../language/dslclause

replace github.com/znasllc-io/memql/component/memql => ../memql

replace github.com/znasllc-io/memql/component/metrics => ../metrics

replace github.com/znasllc-io/memql/component/provenance => ../provenance

replace github.com/znasllc-io/memql/component/safety => ../safety

replace github.com/znasllc-io/memql/component/secret => ../secret

replace github.com/znasllc-io/memql/core => ../../core

replace github.com/znasllc-io/memql/docs => ../../docs

replace github.com/znasllc-io/memql/dsl => ../../dsl

replace github.com/znasllc-io/memql/component/frontdoor => ../frontdoor
