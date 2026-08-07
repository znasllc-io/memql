// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/fileprocessor

go 1.26.1

toolchain go1.26.5

require (
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728
	github.com/znasllc-io/memql/core v0.0.0
)

replace github.com/znasllc-io/memql/core => ../../core
