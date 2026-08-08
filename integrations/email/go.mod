// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/integrations/email

go 1.26.1

toolchain go1.26.5

require (
	github.com/znasllc-io/memql/component/database v0.0.0
	github.com/znasllc-io/memql/component/memql v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
)

require (
	github.com/anthropics/anthropic-sdk-go v1.61.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgraph-io/ristretto v0.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/lib/pq v1.12.3 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1 // indirect
	github.com/sashabaranov/go-openai v1.42.0 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/uptrace/bun v1.2.18 // indirect
	github.com/uptrace/bun/dialect/pgdialect v1.2.18 // indirect
	github.com/uptrace/bun/driver/pgdriver v1.2.18 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/zeozeozeo/gomplerate v0.0.0-20250404113140-0fbb236df825 // indirect
	github.com/znasllc-io/memql/component/actions v0.0.0 // indirect
	github.com/znasllc-io/memql/component/auth v0.0.0 // indirect
	github.com/znasllc-io/memql/component/bus v0.0.0 // indirect
	github.com/znasllc-io/memql/component/bus/gen v0.0.0 // indirect
	github.com/znasllc-io/memql/component/config v0.0.0 // indirect
	github.com/znasllc-io/memql/component/events v0.0.0 // indirect
	github.com/znasllc-io/memql/component/genesis v0.0.0 // indirect
	github.com/znasllc-io/memql/component/grpc/gen v0.0.0 // indirect
	github.com/znasllc-io/memql/component/harness v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/annotations v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/ast v0.0.0 // indirect
	github.com/znasllc-io/memql/component/language/dslclause v0.0.0 // indirect
	github.com/znasllc-io/memql/component/provenance v0.0.0 // indirect
	github.com/znasllc-io/memql/component/safety v0.0.0 // indirect
	github.com/znasllc-io/memql/component/secret v0.0.0 // indirect
	github.com/znasllc-io/memql/docs v0.0.0 // indirect
	github.com/znasllc-io/memql/dsl v0.0.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mellium.im/sasl v0.3.2 // indirect
)

replace github.com/znasllc-io/memql/component/actions => ../../component/actions

replace github.com/znasllc-io/memql/component/auth => ../../component/auth

replace github.com/znasllc-io/memql/component/bus => ../../component/bus

replace github.com/znasllc-io/memql/component/bus/gen => ../../component/bus/gen

replace github.com/znasllc-io/memql/component/config => ../../component/config

replace github.com/znasllc-io/memql/component/database => ../../component/database

replace github.com/znasllc-io/memql/component/events => ../../component/events

replace github.com/znasllc-io/memql/component/genesis => ../../component/genesis

replace github.com/znasllc-io/memql/component/grpc/gen => ../../component/grpc/gen

replace github.com/znasllc-io/memql/component/harness => ../../component/harness

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
