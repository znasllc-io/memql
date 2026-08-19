// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
//
// A leaf: the host algebra is pure string work over the standard library, and
// it must stay that way. component/envregistry derives its issuer and CORS origins
// from the same rule the Ingress generator writes hosts with (memql#3767), so
// anything this module grew a dependency on would be dragged into every module
// that derives a domain.
module github.com/znasllc-io/memql/component/frontdoor

go 1.26.1

toolchain go1.26.6
