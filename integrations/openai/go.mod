// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
module github.com/znasllc-io/memql/integrations/openai

go 1.26.1

toolchain go1.27.1

require (
	github.com/znasllc-io/memql/core v0.0.0
	nhooyr.io/websocket v1.8.17
)

require github.com/zeozeozeo/gomplerate v0.0.0-20250404113140-0fbb236df825 // indirect

replace github.com/znasllc-io/memql/component/actions => ../../component/actions

replace github.com/znasllc-io/memql/component/auth => ../../component/auth

replace github.com/znasllc-io/memql/component/bus => ../../component/bus

replace github.com/znasllc-io/memql/component/bus/gen => ../../component/bus/gen

replace github.com/znasllc-io/memql/component/config => ../../component/config

replace github.com/znasllc-io/memql/component/database => ../../component/database

replace github.com/znasllc-io/memql/component/events => ../../component/events

replace github.com/znasllc-io/memql/component/envregistry => ../../component/envregistry

replace github.com/znasllc-io/memql/component/grpc/gen => ../../component/grpc/gen

replace github.com/znasllc-io/memql/component/language => ../../component/language

replace github.com/znasllc-io/memql/component/language/annotations => ../../component/language/annotations

replace github.com/znasllc-io/memql/component/language/ast => ../../component/language/ast

replace github.com/znasllc-io/memql/component/language/dslclause => ../../component/language/dslclause

replace github.com/znasllc-io/memql/component/memql => ../../component/memql

replace github.com/znasllc-io/memql/component/provenance => ../../component/provenance

replace github.com/znasllc-io/memql/component/safety => ../../component/safety

replace github.com/znasllc-io/memql/component/secret => ../../component/secret

replace github.com/znasllc-io/memql/core => ../../core

replace github.com/znasllc-io/memql/docs => ../../docs

replace github.com/znasllc-io/memql/dsl => ../../dsl

replace github.com/znasllc-io/memql/component/frontdoor => ../../component/frontdoor

replace github.com/znasllc-io/memql/component/metrics => ../../component/metrics
