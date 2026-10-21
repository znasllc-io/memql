// The `wire` tier (memql#3240, design D3 in docs/internal/ops/ci-design.md).
//
// L0: zero dependencies on anything else in this repository. That is what lets
// it be the first module to exist -- every other tier requires something, and
// nothing can be required before its provider is a module.
//
// This is also one of only two modules that will carry an INDEPENDENT version
// line (memql#3245); everything except `wire` and `engine` is lockstep.
module github.com/znasllc-io/memql/component/grpc/gen

go 1.26.8

toolchain go1.27.3

require (
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260803160001-6ac0973c030d // indirect
)
