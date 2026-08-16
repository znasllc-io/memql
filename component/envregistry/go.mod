// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/ci-design.md, section D3.
module github.com/znasllc-io/memql/component/envregistry

go 1.26.1

toolchain go1.26.6

require gopkg.in/yaml.v3 v3.0.1

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/znasllc-io/memql/component/frontdoor v0.0.0
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

replace github.com/znasllc-io/memql/component/frontdoor => ../frontdoor
