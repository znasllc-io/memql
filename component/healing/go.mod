// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/healing

go 1.26.1

toolchain go1.27.1

require (
	github.com/znasllc-io/memql/component/language v0.0.0
	github.com/znasllc-io/memql/component/language/ast v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
)

require (
	github.com/znasllc-io/memql/component/language/annotations v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/dslclause v0.0.0 // indirect
)

replace github.com/znasllc-io/memql/component/language => ../language

replace github.com/znasllc-io/memql/component/language/ast => ../language/ast

replace github.com/znasllc-io/memql/component/language/annotations => ../language/annotations

replace github.com/znasllc-io/memql/component/language/dslclause => ../language/dslclause

replace github.com/znasllc-io/memql/core => ../../core
