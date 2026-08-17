// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/genesis

go 1.26.1

toolchain go1.26.6

require (
	github.com/znasllc-io/memql/component/envregistry v0.0.0
	github.com/znasllc-io/memql/component/secret v0.0.0
)

require (
	github.com/kr/text v0.2.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

require (
	github.com/znasllc-io/memql/component/frontdoor v0.0.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/znasllc-io/memql/component/envregistry => ../envregistry

replace github.com/znasllc-io/memql/component/secret => ../secret

replace github.com/znasllc-io/memql/component/frontdoor => ../frontdoor
