// The `wire` tier (memql#3240, design D3 in docs/ci-design.md).
//
// L0: zero dependencies on anything else in this repository. That is what lets
// it be the first module to exist -- every other tier requires something, and
// nothing can be required before its provider is a module.
//
// This is also one of only two modules that will carry an INDEPENDENT version
// line (memql#3245); everything except `wire` and `engine` is lockstep.
module github.com/znasllc-io/memql/component/grpc/gen

go 1.26.1

toolchain go1.26.5

require (
	google.golang.org/grpc v1.81.0-dev
	google.golang.org/protobuf v1.36.11
)

require (
	go.opentelemetry.io/otel v1.41.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
