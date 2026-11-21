// The pure decision rules of a generic calls capability: who is admitted to a
// call, who waits, in what order a queue drains, what happens when the host
// leaves and whether a schedule is acceptable. Values in, decisions out.
// Production imports remain standard-library only (purity_test.go).
// Test-only requirements support the workflow rule regression suite.
module github.com/znasllc-io/memql/component/calls

go 1.26.8

toolchain go1.27.4

require github.com/stretchr/testify v1.12.1

require go.yaml.in/yaml/v3 v3.0.5 // indirect
