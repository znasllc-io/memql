// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/actions

go 1.26.1

toolchain go1.26.5

require (
	github.com/znasllc-io/memql/component/harness v0.0.0
	github.com/znasllc-io/memql/component/language v0.0.0
	github.com/znasllc-io/memql/component/language/ast v0.0.0
	github.com/znasllc-io/memql/dsl v0.0.0
)

require (
	github.com/znasllc-io/memql/component/language/annotations v0.0.0 // indirect
	github.com/znasllc-io/memql/core v0.0.0 // indirect
)

replace github.com/znasllc-io/memql/component/harness => ../harness

replace github.com/znasllc-io/memql/component/language => ../language

replace github.com/znasllc-io/memql/component/language/annotations => ../language/annotations

replace github.com/znasllc-io/memql/component/language/ast => ../language/ast

replace github.com/znasllc-io/memql/core => ../../core

replace github.com/znasllc-io/memql/dsl => ../../dsl
