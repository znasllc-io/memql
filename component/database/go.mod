// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/database

go 1.26.1

toolchain go1.26.5

require (
	github.com/lib/pq v1.12.3
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1
	github.com/stretchr/testify v1.11.1
	github.com/uptrace/bun v1.2.18
	github.com/uptrace/bun/dialect/pgdialect v1.2.18
	github.com/uptrace/bun/driver/pgdriver v1.2.18
	github.com/znasllc-io/memql/component/language v0.0.0
	github.com/znasllc-io/memql/component/language/ast v0.0.0
	github.com/znasllc-io/memql/component/provenance v0.0.0
	github.com/znasllc-io/memql/core v0.0.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/znasllc-io/memql/component/language/annotations v0.0.0 // indirect
	go.opentelemetry.io/otel v1.41.0 // indirect
	go.opentelemetry.io/otel/trace v1.41.0 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mellium.im/sasl v0.3.2 // indirect
)

replace github.com/znasllc-io/memql/component/language => ../language

replace github.com/znasllc-io/memql/component/language/annotations => ../language/annotations

replace github.com/znasllc-io/memql/component/language/ast => ../language/ast

replace github.com/znasllc-io/memql/component/provenance => ../provenance

replace github.com/znasllc-io/memql/core => ../../core
