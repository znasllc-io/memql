// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/observe

go 1.26.1

toolchain go1.26.5

require (
	github.com/uptrace/bun v1.2.18
	github.com/znasllc-io/memql/component/events v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
)

require (
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/znasllc-io/memql/component/bus v0.0.0 // indirect
	github.com/znasllc-io/memql/component/bus/gen v0.0.0 // indirect
	github.com/znasllc-io/memql/component/grpc/gen v0.0.0 // indirect
	go.opentelemetry.io/otel/metric v1.41.0 // indirect
	go.opentelemetry.io/otel/trace v1.41.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.81.0-dev // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/znasllc-io/memql/component/bus => ../bus

replace github.com/znasllc-io/memql/component/bus/gen => ../bus/gen

replace github.com/znasllc-io/memql/component/events => ../events

replace github.com/znasllc-io/memql/component/grpc/gen => ../grpc/gen

replace github.com/znasllc-io/memql/core => ../../core
