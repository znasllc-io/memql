// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/metadata

go 1.26.1

toolchain go1.26.5

require (
	github.com/oschwald/geoip2-golang v1.13.0
	github.com/znasllc-io/memql/component/auth v0.0.0
)

require (
	github.com/oschwald/maxminddb-golang v1.13.0 // indirect
	github.com/znasllc-io/memql/core v0.0.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/znasllc-io/memql/component/auth => ../auth

replace github.com/znasllc-io/memql/core => ../../core
