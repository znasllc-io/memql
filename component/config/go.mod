// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/config

go 1.26.1

toolchain go1.26.5

require (
	github.com/znasllc-io/memql/component/bus/gen v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
)

require google.golang.org/protobuf v1.36.11 // indirect

replace github.com/znasllc-io/memql/component/bus/gen => ../bus/gen

replace github.com/znasllc-io/memql/core => ../../core
