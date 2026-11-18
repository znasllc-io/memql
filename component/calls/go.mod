// The pure decision rules of a generic calls capability: who is admitted to a
// call, who waits, in what order a queue drains, what happens when the host
// leaves and whether a schedule is acceptable. Values in, decisions out.
// It has NO requires and must keep none: see purity_test.go.
module github.com/znasllc-io/memql/component/calls

go 1.26.8

toolchain go1.27.4
