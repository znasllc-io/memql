// The `wire` tier (memql#3240, design D3 in docs/ci-design.md).
//
// L0: zero dependencies on anything else in this repository. See
// component/grpc/gen/go.mod for the tier's rationale.
//
// Narrower than its two siblings: the bus protos carry no service definitions,
// so this one needs protobuf but not grpc.
module github.com/znasllc-io/memql/component/bus/gen

go 1.26.1

toolchain go1.26.5

require google.golang.org/protobuf v1.36.11
