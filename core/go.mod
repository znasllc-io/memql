// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
module github.com/znasllc-io/memql/core

go 1.26.1

toolchain go1.27.1

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.12.1
	github.com/zeozeozeo/gomplerate v0.0.0-20250404113140-0fbb236df825
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect
