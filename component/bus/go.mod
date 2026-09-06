// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/bus

go 1.26.1

toolchain go1.27.1

require (
	github.com/znasllc-io/memql/component/bus/gen v0.0.0
	google.golang.org/protobuf v1.36.12
)

replace github.com/znasllc-io/memql/component/bus/gen => ./gen
