// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/safety

go 1.26.1

toolchain go1.26.5

require github.com/znasllc-io/memql/core v0.0.0

replace github.com/znasllc-io/memql/core => ../../core
