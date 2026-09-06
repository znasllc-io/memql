// The `wire` tier (memql#3240, design D3 in docs/internal/ops/ci-design.md).
//
// L0: zero dependencies on anything else in this repository. That is what lets
// it be the first module to exist -- every other tier requires something, and
// nothing can be required before its provider is a module.
//
// This is also one of only two modules that will carry an INDEPENDENT version
// line (memql#3245); everything except `wire` and `engine` is lockstep.
module github.com/znasllc-io/memql/component/grpc/gen

go 1.26.1

toolchain go1.27.1

require (
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
