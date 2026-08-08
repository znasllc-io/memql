// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/language

go 1.26.1

toolchain go1.26.5

require (
	github.com/znasllc-io/memql/component/language/annotations v0.0.0
	github.com/znasllc-io/memql/component/language/ast v0.0.0
	github.com/znasllc-io/memql/component/language/dslclause v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
)

replace github.com/znasllc-io/memql/component/language/annotations => ./annotations

replace github.com/znasllc-io/memql/component/language/ast => ./ast

replace github.com/znasllc-io/memql/component/language/dslclause => ./dslclause

replace github.com/znasllc-io/memql/core => ../../core
