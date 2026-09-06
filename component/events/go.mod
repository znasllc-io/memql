// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/events

go 1.26.1

toolchain go1.27.1

require (
	github.com/znasllc-io/memql/component/bus v0.0.0
	github.com/znasllc-io/memql/component/bus/gen v0.0.0
	github.com/znasllc-io/memql/component/grpc/gen v0.0.0-00010101000000-000000000000
	github.com/znasllc-io/memql/core v0.0.0
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.2 // indirect
)

replace github.com/znasllc-io/memql/component/bus => ../bus

replace github.com/znasllc-io/memql/component/bus/gen => ../bus/gen

replace github.com/znasllc-io/memql/component/grpc/gen => ../grpc/gen

replace github.com/znasllc-io/memql/core => ../../core
