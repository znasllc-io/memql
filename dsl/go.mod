// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
module github.com/znasllc-io/memql/dsl

go 1.26.8

toolchain go1.27.2

require github.com/znasllc-io/memql/core v0.0.0

replace github.com/znasllc-io/memql/core => ../core
